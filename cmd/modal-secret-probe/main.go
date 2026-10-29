// Command modal-secret-probe resolves one account's Modal grant out of each region's data plane
// config bucket, through exactly the code path the worker controller uses, and reports what it
// found without printing any of it.
//
// It exists because "WCI can read and decrypt its region's sealed material" is otherwise only
// observable by deploying a cell and scaling a worker up, which answers the question slowly and
// confounds it with everything else that has to be right. This asks only that question, against
// the real buckets and the real KMS keys, and it answers it per region so a cloud or a region that
// is not wired up names itself.
//
//	go run ./cmd/modal-secret-probe \
//	  -targets aws/us-east-1,aws/us-west-2,gcp/us-central1,gcp/us-west1 \
//	  -account temporal-dev -workspace ac-B09IwjAEGqV3UBHurfXzWM \
//	  -client-id oc-FLiPU6I6L1BINlXgIBba4D
//
// Credentials come from the ambient environment, so it needs whatever the shell is already
// authenticated as to be able to read the bucket and call KMS Decrypt in each region probed.
package main

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"flag"
	"fmt"
	"os"
	"strings"
	"time"

	"go.uber.org/zap"

	computeprovider "go.temporal.io/auto-scaled-workers/wci/workflow/compute_provider"
)

func main() {
	var (
		targets      = flag.String("targets", "", "comma-separated cloud/region pairs, e.g. aws/us-west-2,gcp/us-central1")
		accountID    = flag.String("account", "", "account id the grant belongs to")
		workspaceID  = flag.String("workspace", "", "Modal workspace id the grant is for")
		clientID     = flag.String("client-id", "", "Modal OAuth client id the grant was issued to")
		bucketFormat = flag.String("bucket-format", "data-plane-config-test-%s", "regional bucket name, %s is the region")
		roleFormat   = flag.String("role-format", "arn:aws:iam::730335288748:role/data-plane-config-test-%s-reader", "AWS role to assume to read the bucket, %s is the region")
		prefix       = flag.String("prefix", "", "object prefix in the bucket; empty uses the control plane's default")
		awsKeyID     = flag.String("aws-key-id", "", "KMS key ARN AWS objects must name; empty accepts whatever they name")
		gcpKeyID     = flag.String("gcp-key-id", "", "Cloud KMS cryptoKey GCP objects must name, without its version; empty accepts whatever they name")
		verbose      = flag.Bool("verbose", false, "log the object store client's own output")
	)
	flag.Parse()

	if *targets == "" || *accountID == "" || *workspaceID == "" || *clientID == "" {
		fmt.Fprintln(os.Stderr, "-targets, -account, -workspace and -client-id are all required")
		flag.Usage()
		os.Exit(2)
	}

	log := zap.NewNop()
	if *verbose {
		log, _ = zap.NewDevelopment()
	}

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()

	failed := 0
	for _, target := range strings.Split(*targets, ",") {
		cloud, region, ok := strings.Cut(strings.TrimSpace(target), "/")
		if !ok {
			fmt.Printf("FAIL  %-28s malformed target, want cloud/region\n", target)
			failed++
			continue
		}

		cfg := computeprovider.ModalStoreConfig{
			CloudProvider: cloud,
			Region:        region,
			Bucket:        fmt.Sprintf(*bucketFormat, region),
			ObjectPrefix:  *prefix,
			OAuthClientID: *clientID,
		}
		// One key per cloud, because a worker controller unwraps through its own cloud's KMS and a
		// key is only usable in the half of the fleet it belongs to.
		switch cloud {
		case "aws":
			// An empty -role-format reaches the bucket as your own identity, which is what an
			// account whose reader role you cannot assume (or cannot grant kms:Decrypt) needs.
			if *roleFormat != "" {
				cfg.AssumeRoleARN = fmt.Sprintf(*roleFormat, region)
			}
			cfg.SealingKeyID = *awsKeyID
		case "gcp":
			cfg.SealingKeyID = *gcpKeyID
		}

		if err := probe(ctx, cfg, *accountID, *workspaceID, log); err != nil {
			fmt.Printf("FAIL  %-28s %v\n", target, err)
			failed++
			continue
		}
	}

	if failed > 0 {
		fmt.Printf("\n%d of %d targets failed\n", failed, len(strings.Split(*targets, ",")))
		os.Exit(1)
	}
	fmt.Println("\nall targets read and decrypted")
}

func probe(ctx context.Context, cfg computeprovider.ModalStoreConfig, accountID, workspaceID string, log *zap.Logger) error {
	source, err := computeprovider.NewObjectStoreModalSecretSource(ctx, cfg, log)
	if err != nil {
		return err
	}
	if closer, ok := source.(interface{ Close() error }); ok {
		defer func() { _ = closer.Close() }()
	}

	start := time.Now()
	creds, err := source.Credentials(ctx, accountID, workspaceID)
	if err != nil {
		return err
	}
	if creds.OAuth == nil {
		return fmt.Errorf("resolved credentials carry no OAuth grant")
	}

	// Fingerprints, never the material. The point is to show that two regions hold the same grant
	// and the same signing key, which a digest settles as well as the secret would.
	fmt.Printf("OK    %-5s %-14s %-34s token %s  key %s (%d bytes)  %s\n",
		cfg.CloudProvider,
		cfg.Region,
		cfg.Bucket,
		fingerprint(creds.OAuth.RefreshToken),
		fingerprint(creds.OAuth.JWTKey),
		len(creds.OAuth.JWTKey),
		time.Since(start).Round(time.Millisecond),
	)
	return nil
}

func fingerprint(secret string) string {
	sum := sha256.Sum256([]byte(secret))
	return hex.EncodeToString(sum[:])[:12]
}
