package transcript

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"time"

	"github.com/klauspost/compress/zstd"
)

// segmentUpload tracks one segment's staged blocks and its commit.
type segmentUpload struct {
	// guarded by uploadPool.mu
	pending    int
	failed     bool
	commitTask *uploadTask
	// guarded by uploadPool.rowsMu
	stored  bool
	lost    bool
	rows    []*callRow
	onDone  []func(ok bool)
	settled bool
}

func newSegmentUpload() *segmentUpload { return &segmentUpload{} }

type uploadKind int

const (
	uploadBlock uploadKind = iota
	uploadCommit
	uploadBlob
)

type uploadTask struct {
	kind     uploadKind
	seg      *sessionSegment
	upload   *segmentUpload
	blob, id string
	data     []byte
	meta     map[string]string
	size     int64
}

// uploadPool runs block uploads, commits and index writes on a few
// goroutines, retrying each with backoff until it is stored or the pool
// stops. Its byte backlog is what the writer checks before admitting events.
type uploadPool struct {
	w      *segmentWriter
	mu     sync.Mutex
	cond   *sync.Cond
	tasks  []*uploadTask
	active int
	ctx    context.Context
	cancel context.CancelFunc
	wg     sync.WaitGroup
	rowsMu sync.Mutex

	backlog        atomic.Int64
	committed      atomic.Int64
	committedBytes atomic.Int64
	committedRaw   atomic.Int64
	committedBody  atomic.Int64
	lost           atomic.Int64
	retries        atomic.Int64
	indexParts     atomic.Int64
	indexRows      atomic.Int64
}

func newUploadPool(w *segmentWriter) *uploadPool {
	ctx, cancel := context.WithCancel(context.Background())
	pool := &uploadPool{w: w, ctx: ctx, cancel: cancel}
	pool.cond = sync.NewCond(&pool.mu)
	for i := 0; i < w.cfg.UploadWorkers; i++ {
		pool.wg.Add(1)
		go pool.work()
	}
	return pool
}

func (p *uploadPool) full() bool { return p.backlog.Load() >= p.w.cfg.UploadBacklogBytes }

func (p *uploadPool) push(task *uploadTask) {
	p.backlog.Add(task.size)
	p.mu.Lock()
	p.tasks = append(p.tasks, task)
	p.mu.Unlock()
	p.cond.Broadcast()
}

func (p *uploadPool) putBlock(upload *segmentUpload, blob, id string, data []byte) {
	p.mu.Lock()
	upload.pending++
	p.mu.Unlock()
	p.push(&uploadTask{kind: uploadBlock, upload: upload, blob: blob, id: id, data: data, size: int64(len(data))})
}

func (p *uploadPool) putBlob(blob string, data []byte, contentType string) {
	p.push(&uploadTask{kind: uploadBlob, blob: blob, data: data, meta: map[string]string{"content_type": contentType}, size: int64(len(data))})
}

// commit queues the block list once every block of the segment is staged.
func (p *uploadPool) commit(seg *sessionSegment, metadata map[string]string) {
	task := &uploadTask{kind: uploadCommit, seg: seg, upload: seg.upload, blob: seg.blob, meta: metadata}
	p.mu.Lock()
	if seg.upload.pending > 0 {
		seg.upload.commitTask = task
		p.mu.Unlock()
		return
	}
	p.mu.Unlock()
	p.push(task)
}

func (p *uploadPool) work() {
	defer p.wg.Done()
	for {
		p.mu.Lock()
		for len(p.tasks) == 0 && p.ctx.Err() == nil {
			p.cond.Wait()
		}
		if len(p.tasks) == 0 {
			p.mu.Unlock()
			return
		}
		task := p.tasks[0]
		p.tasks = p.tasks[1:]
		p.active++
		p.mu.Unlock()

		p.run(task)

		p.mu.Lock()
		p.active--
		p.mu.Unlock()
		p.cond.Broadcast()
	}
}

func (p *uploadPool) run(task *uploadTask) {
	defer p.backlog.Add(-task.size)
	switch task.kind {
	case uploadBlock:
		err := p.retry(func(ctx context.Context) error {
			return p.w.cfg.Uploader.PutBlock(ctx, task.blob, task.id, task.data)
		})
		task.data = nil
		p.mu.Lock()
		task.upload.pending--
		if err != nil {
			task.upload.failed = true
		}
		var commit *uploadTask
		if task.upload.pending == 0 && task.upload.commitTask != nil {
			commit = task.upload.commitTask
			task.upload.commitTask = nil
		}
		p.mu.Unlock()
		if err != nil {
			p.w.cfg.Logger.Warn("transcript block upload failed", "blob", task.blob, "error", err)
		}
		if commit != nil {
			p.push(commit)
		}
	case uploadCommit:
		p.mu.Lock()
		failed := task.upload.failed
		p.mu.Unlock()
		ok := false
		if !failed {
			err := p.retry(func(ctx context.Context) error {
				return p.w.cfg.Uploader.PutBlockList(ctx, task.blob, task.seg.blockIDs, "application/zstd", task.meta)
			})
			if err == nil {
				ok = true
			} else {
				p.w.cfg.Logger.Warn("transcript segment commit failed", "blob", task.blob, "error", err)
			}
		}
		if ok {
			p.committed.Add(1)
			p.committedBytes.Add(task.seg.compressed)
			p.committedRaw.Add(task.seg.rawTotal)
			p.committedBody.Add(task.seg.bodyBytes)
		} else {
			p.lost.Add(1)
		}
		p.settle(task.seg, ok)
	case uploadBlob:
		err := p.retry(func(ctx context.Context) error {
			return p.w.cfg.Uploader.PutBlob(ctx, task.blob, task.data, task.meta["content_type"])
		})
		task.data = nil
		if err == nil {
			p.indexParts.Add(1)
		} else {
			p.w.cfg.Logger.Warn("transcript index upload failed", "blob", task.blob, "error", err)
		}
	}
}

// PermanentError marks an upload failure that a retry cannot fix (refused
// credential, malformed request).
type PermanentError struct{ Err error }

func (e PermanentError) Error() string { return e.Err.Error() }
func (e PermanentError) Unwrap() error { return e.Err }

// retry runs fn until it succeeds, fails permanently, or the pool stops.
func (p *uploadPool) retry(fn func(context.Context) error) error {
	for attempt := 0; ; attempt++ {
		ctx, cancel := context.WithTimeout(p.ctx, 5*time.Minute)
		err := fn(ctx)
		cancel()
		if err == nil {
			return nil
		}
		var permanent PermanentError
		if errors.As(err, &permanent) || p.ctx.Err() != nil {
			return err
		}
		p.retries.Add(1)
		wait := time.Second << min(attempt, 5)
		timer := time.NewTimer(wait)
		select {
		case <-p.ctx.Done():
			timer.Stop()
			return err
		case <-timer.C:
		}
	}
}

// settle records a segment's outcome and releases the rows waiting on it.
func (p *uploadPool) settle(seg *sessionSegment, ok bool) {
	p.rowsMu.Lock()
	upload := seg.upload
	upload.settled, upload.stored, upload.lost = true, ok, !ok
	rows := upload.rows
	upload.rows = nil
	callbacks := upload.onDone
	upload.onDone = nil
	var ready []IndexRow
	for _, call := range rows {
		if ok {
			resolvePart(call, seg)
		} else {
			call.row.BodyLost = true
		}
		call.waiting--
		if call.waiting == 0 {
			ready = append(ready, call.row)
		}
	}
	p.rowsMu.Unlock()
	for _, row := range ready {
		p.w.emitRow(row)
	}
	for _, callback := range callbacks {
		callback(ok)
	}
}

// attachRow hands a finished call to the segments its ranges point into.
func (p *uploadPool) attachRow(call *callRow) {
	p.rowsMu.Lock()
	seen := map[*sessionSegment]bool{}
	for _, part := range []*partRef{&call.req, &call.resp} {
		if !part.set || seen[part.seg] {
			continue
		}
		seen[part.seg] = true
		upload := part.seg.upload
		if upload.settled {
			if upload.stored {
				resolvePart(call, part.seg)
			} else {
				call.row.BodyLost = true
			}
			continue
		}
		call.waiting++
		upload.rows = append(upload.rows, call)
	}
	ready := call.waiting == 0
	p.rowsMu.Unlock()
	if ready {
		p.w.emitRow(call.row)
	}
}

// onSettled runs fn once the segment is stored or lost.
func (p *uploadPool) onSettled(seg *sessionSegment, fn func(ok bool)) {
	p.rowsMu.Lock()
	if seg.upload.settled {
		ok := seg.upload.stored
		p.rowsMu.Unlock()
		fn(ok)
		return
	}
	seg.upload.onDone = append(seg.upload.onDone, fn)
	p.rowsMu.Unlock()
}

// waitIdle returns when no task is queued or running, or ctx ends.
func (p *uploadPool) waitIdle(ctx context.Context) {
	stop := context.AfterFunc(ctx, func() { p.cond.Broadcast() })
	defer stop()
	p.mu.Lock()
	defer p.mu.Unlock()
	for (len(p.tasks) > 0 || p.active > 0 || p.waitingCommits()) && ctx.Err() == nil {
		p.cond.Wait()
	}
}

// waitingCommits reports commits parked behind outstanding blocks; caller
// holds p.mu. Parked commits are queued by the block that finishes last, so
// pending work is visible through tasks and active alone.
func (p *uploadPool) waitingCommits() bool { return false }

// waitRoom blocks the offline converter while the backlog is full.
func (p *uploadPool) waitRoom(ctx context.Context) {
	stop := context.AfterFunc(ctx, func() { p.cond.Broadcast() })
	defer stop()
	p.mu.Lock()
	defer p.mu.Unlock()
	for p.full() && ctx.Err() == nil {
		p.cond.Wait()
	}
}

func (p *uploadPool) stop() {
	p.cancel()
	p.cond.Broadcast()
	p.wg.Wait()
}

func newIndexEncoder() (*zstd.Encoder, error) {
	return zstd.NewWriter(nil, zstd.WithEncoderLevel(zstd.SpeedBestCompression), zstd.WithEncoderConcurrency(1))
}
