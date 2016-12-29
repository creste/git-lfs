package commands

import (
	"os"
	"sync"

	"github.com/git-lfs/git-lfs/errors"
	"github.com/git-lfs/git-lfs/lfs"
	"github.com/git-lfs/git-lfs/progress"
	"github.com/git-lfs/git-lfs/tools"
	"github.com/git-lfs/git-lfs/tq"
)

var uploadMissingErr = "%s does not exist in .git/lfs/objects. Tried %s, which matches %s."

type uploadContext struct {
	DryRun       bool
	uploadedOids tools.StringSet

	meter progress.Meter
	tq    *tq.TransferQueue

	cwg *sync.WaitGroup
	cq  *tq.TransferQueue
}

func newUploadContext(dryRun bool) *uploadContext {
	ctx := &uploadContext{
		DryRun:       dryRun,
		uploadedOids: tools.NewStringSet(),

		cwg: new(sync.WaitGroup),
		// TODO(taylor): single item batches are needed to enqueue each
		// item immediately to avoid waiting an infinite amount of time
		// on an underfilled batch.
		cq: newDownloadCheckQueue(tq.WithBatchSize(1)),
	}

	ctx.cq.Notify(func(oid string, ok bool) {
		if ok {
			// If the object was "ok", the server already has it,
			// and can be marked as uploaded.
			ctx.SetUploaded(oid)
		}

		// No matter whether or not the sever has the object, mark this
		// OID as checked.
		ctx.cwg.Done()
	})

	ctx.meter = buildProgressMeter(ctx.DryRun)
	ctx.tq = newUploadQueue(tq.WithProgress(ctx.meter), tq.DryRun(ctx.DryRun))

	return ctx
}

// AddUpload adds the given oid to the set of oids that have been uploaded in
// the current process.
func (c *uploadContext) SetUploaded(oid string) {
	c.uploadedOids.Add(oid)
}

// HasUploaded determines if the given oid has already been uploaded in the
// current process.
func (c *uploadContext) HasUploaded(oid string) bool {
	return c.uploadedOids.Contains(oid)
}

func (c *uploadContext) prepareUpload(unfiltered ...*lfs.WrappedPointer) (*tq.TransferQueue, []*lfs.WrappedPointer) {
	numUnfiltered := len(unfiltered)
	uploadables := make([]*lfs.WrappedPointer, 0, numUnfiltered)
	missingLocalObjects := make([]*lfs.WrappedPointer, 0, numUnfiltered)
	missingSize := int64(0)

	// XXX(taylor): temporary measure to fix duplicate (broken) results from
	// scanner
	uniqOids := tools.NewStringSet()

	// separate out objects that _should_ be uploaded, but don't exist in
	// .git/lfs/objects. Those will skipped if the server already has them.
	for _, p := range unfiltered {
		// object already uploaded in this process, or we've already
		// seen this OID (see above), skip!
		if uniqOids.Contains(p.Oid) || c.HasUploaded(p.Oid) {
			continue
		}
		uniqOids.Add(p.Oid)

		// estimate in meter early (even if it's not going into uploadables), since
		// we will call Skip() based on the results of the download check queue.
		c.meter.Add(p.Size)

		if lfs.ObjectExistsOfSize(p.Oid, p.Size) {
			uploadables = append(uploadables, p)
		} else {
			// We think we need to push this but we don't have it
			// Store for server checking later
			missingLocalObjects = append(missingLocalObjects, p)
			missingSize += p.Size
		}
	}

	// check to see if the server has the missing objects.
	c.checkMissing(missingLocalObjects, missingSize)

	// use the context's TransferQueue, automatically skipping any missing
	// objects that the server already has.
	for _, p := range missingLocalObjects {
		if c.HasUploaded(p.Oid) {
			// if the server already has this object, call Skip() on
			// the progressmeter to decrement the number of files by
			// 1 and the number of bytes by `p.Size`.
			c.tq.Skip(p.Size)
		} else {
			uploadables = append(uploadables, p)
		}
	}

	return c.tq, uploadables
}

// This checks the given slice of pointers that don't exist in .git/lfs/objects
// against the server. Anything the server already has does not need to be
// uploaded again.
func (c *uploadContext) checkMissing(missing []*lfs.WrappedPointer, missingSize int64) {
	c.cwg.Add(len(missing))
	for _, p := range missing {
		c.cq.Add(downloadTransfer(p))
	}

	c.cwg.Wait()
}

func uploadPointers(c *uploadContext, unfiltered ...*lfs.WrappedPointer) {
	if c.DryRun {
		for _, p := range unfiltered {
			if c.HasUploaded(p.Oid) {
				continue
			}

			Print("push %s => %s", p.Oid, p.Name)
			c.SetUploaded(p.Oid)
		}

		return
	}

	q, pointers := c.prepareUpload(unfiltered...)
	for _, p := range pointers {
		t, err := uploadTransfer(p.Oid, p.Name)
		if err != nil {
			if errors.IsCleanPointerError(err) {
				Exit(uploadMissingErr, p.Oid, p.Name, errors.GetContext(err, "pointer").(*lfs.Pointer).Oid)
			} else {
				ExitWithError(err)
			}
		}

		q.Add(t.Name, t.Path, t.Oid, t.Size)
		c.SetUploaded(p.Oid)
	}
}

func (c *uploadContext) Await() {
	c.tq.Wait()

	for _, err := range c.tq.Errors() {
		FullError(err)
	}

	if len(c.tq.Errors()) > 0 {
		os.Exit(2)
	}
}
