package agent

// newStreamClient returns a Client in objectTransfer: controller mode. The
// stub servers of the older tests serve the controller's streaming artifact
// routes only, so their clients must not ask for presigned URLs.
func newStreamClient(baseURL, token string) *Client {
	c := NewClient(baseURL, token)
	c.SetObjectTransferController(true)
	return c
}
