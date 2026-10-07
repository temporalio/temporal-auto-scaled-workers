package hostconfig

// Config holds static facts about where this worker controller is hosted.
type Config struct {
	RegionID string // <cloud provider>-<region>, e.g. "aws-us-west-2"
}
