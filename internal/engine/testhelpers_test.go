package engine

import "context"

// Helpers only tests call. They wrap or compose production code, and live
// here so the shipped binary carries no function nothing reaches.

// Describe renders the choreography as the exact command list with runtime
// branches as branches. Placeholders <old>/<new> resolve at apply time.
func (e *Engine) Describe(remoteCompose string) []string {
	return e.DescribeWorkloadPlans(remoteCompose, nil)
}

// Abort reverts an interrupted deploy to the previous release. The migration
// gate governs abort exactly like auto-rollback: aborting
// after a schema change is the same hazard.
//
// force is that gate and nothing else. A previous release whose snapshot cannot
// be read is not a gate an operator may assert past: without it there is no
// record of that release's choreography, so there is nothing to replay.
func (e *Engine) Abort(ctx context.Context, force bool) error {
	_, err := e.AbortWithJournalID(ctx, force)
	return err
}

// Rollback re-releases the previous release dir: its compose.yaml pins the
// old image locally because rollback never pulls, and its own ob.yml
// snapshot drives the choreography — old release, old config, old modes.
func (e *Engine) Rollback(ctx context.Context) error {
	_, err := e.RollbackWithJournalID(ctx)
	return err
}

// SecretsPushBatch rotates one complete declaration graph. It never exposes a
// content hash: comparison happens with cmp on the host after an opaque upload,
// and journals contain generation identifiers only.
func (e *Engine) SecretsPushBatch(ctx context.Context, payloads []SecretPayload) (result SecretPushResult, err error) {
	return e.SecretsPushBatchWithInputs(ctx, payloads, nil)
}

// ExactServiceImageCached verifies Docker has the requested immutable image
// and that Docker associates it with the requested manifest digest. Docker's
// .Id is a configuration-object digest, not the manifest/index digest carried
// by an OCI @sha256 reference, so only RepoDigests are acceptable evidence.
func (e *Engine) ExactServiceImageCached(ctx context.Context, image string) (bool, error) {
	return ExactServiceImageCached(ctx, e.T, image)
}
