// Package modal is the compute provider that runs Temporal workers as Modal Function calls.
//
// By default the Modal SDK finds its own credentials. When the deployment configures a data plane
// config bucket, each namespace's account uses its own Modal OAuth grant, which the control plane
// writes to the bucket sealed to a KMS key.
package modal
