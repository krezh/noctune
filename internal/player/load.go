package player

import (
	"context"
	"errors"
	"slices"
	"sync"
)

// LoadSpec describes a search or playlist being resolved into the queue.
// Progress counts source entries already queued, so a load interrupted by a
// restart resumes after them.
type LoadSpec struct {
	Query                string `json:"query"`
	RequestedBy          string `json:"requestedBy"`
	RequestedByAvatarURL string `json:"requestedByAvatarUrl,omitempty"`
	PlayNext             bool   `json:"playNext,omitempty"`
	Multi                bool   `json:"multi,omitempty"`
	Progress             int    `json:"progress,omitempty"`
	LastID               string `json:"lastId,omitempty"`
}

// Load is one in-flight resolution registered with a GuildPlayer.
type Load struct {
	gp         *GuildPlayer
	ctx        context.Context
	id         uint64
	generation uint64
	spec       *LoadSpec
	finish     func()
	finishOnce sync.Once
}

// BeginLoad registers a resolution for generation. It fails if Stop or
// Leave has invalidated that generation. Finish must be called when done.
func (gp *GuildPlayer) BeginLoad(parent context.Context, generation uint64, spec LoadSpec) (*Load, bool) {
	ctx, finish, ok := gp.BeginResolution(parent, generation)
	if !ok {
		return nil, false
	}
	gp.mu.Lock()
	l := gp.registerLoadLocked(spec)
	gp.mu.Unlock()
	l.ctx, l.generation, l.finish = ctx, generation, finish
	gp.notify()
	return l, true
}

func (gp *GuildPlayer) registerLoadLocked(spec LoadSpec) *Load {
	if gp.loads == nil {
		gp.loads = make(map[uint64]*LoadSpec)
	}
	gp.loadSeq++
	s := spec
	gp.loads[gp.loadSeq] = &s
	if s.Multi {
		gp.loadingCount++
	}
	return &Load{gp: gp, id: gp.loadSeq, spec: &s}
}

// BeginRestoredLoads restarts loads that were in flight when the previous
// process shut down.
func (gp *GuildPlayer) BeginRestoredLoads(parent context.Context) []*Load {
	gp.mu.Lock()
	specs := gp.restoredLoads
	gp.restoredLoads = nil
	generation := gp.resolutionGen
	loads := make([]*Load, 0, len(specs))
	for _, spec := range specs {
		loads = append(loads, gp.registerLoadLocked(spec))
	}
	gp.mu.Unlock()

	started := loads[:0]
	for _, l := range loads {
		ctx, finish, ok := gp.BeginResolution(parent, generation)
		if !ok {
			l.drop()
			continue
		}
		l.ctx, l.generation, l.finish = ctx, generation, finish
		started = append(started, l)
	}
	gp.notify()
	return started
}

func (l *Load) Context() context.Context { return l.ctx }

// Spec returns the load's current description, including its progress.
func (l *Load) Spec() LoadSpec {
	l.gp.mu.Lock()
	defer l.gp.mu.Unlock()
	return *l.spec
}

// Add queues track, the index'th entry of the load's source, and records
// that progress in the same step so a restart neither loses nor repeats it.
func (l *Load) Add(track *Track, index int) error {
	gp := l.gp
	gp.mu.Lock()
	if l.generation != gp.resolutionGen {
		gp.mu.Unlock()
		return context.Canceled
	}
	err := gp.checkCapacityLocked()
	if err == nil {
		if l.spec.PlayNext {
			idx := 0
			if l.spec.LastID != "" {
				for i, t := range gp.queue {
					if t.ID == l.spec.LastID {
						idx = i + 1
						break
					}
				}
			}
			gp.queue = slices.Insert(gp.queue, idx, track)
		} else {
			gp.queue = slices.Insert(gp.queue, fairInsertIndex(gp.current, gp.queue, track.RequestedBy), track)
		}
		l.spec.Progress = index + 1
		l.spec.LastID = track.ID
	}
	gp.mu.Unlock()
	if err != nil {
		return err
	}
	gp.notify()

	select {
	case gp.playSignal <- struct{}{}:
	default:
	}
	return nil
}

// Finish ends the load. A load canceled from outside (process shutdown)
// stays recorded so the next start resumes it; one ended by Stop or Leave,
// by completing, or by failing is forgotten.
func (l *Load) Finish(err error) {
	l.finishOnce.Do(func() {
		l.finish()
		gp := l.gp
		gp.mu.Lock()
		keep := errors.Is(err, context.Canceled) && l.generation == gp.resolutionGen
		l.untrackLocked(keep)
		gp.mu.Unlock()
		gp.notify()
	})
}

func (l *Load) drop() {
	l.gp.mu.Lock()
	l.untrackLocked(false)
	l.gp.mu.Unlock()
}

func (l *Load) untrackLocked(keep bool) {
	gp := l.gp
	if gp.loads[l.id] != l.spec {
		return
	}
	if l.spec.Multi {
		gp.loadingCount--
	}
	if keep {
		gp.restoredLoads = append(gp.restoredLoads, *l.spec)
	}
	delete(gp.loads, l.id)
}

func (gp *GuildPlayer) clearLoadsLocked() {
	gp.loads = nil
	gp.restoredLoads = nil
	gp.loadingCount = 0
}
