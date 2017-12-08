package routing

import (
	"sync"

	"github.com/lightningnetwork/lnd/channeldb"
	"github.com/lightningnetwork/lnd/lnwire"
)

// ValidationBarrier is a barrier used to ensure proper validation order while
// concurrently validating new announcements for channel edges, and the
// attributes of channel edges.  It uses this set of maps (protected by this
// mutex) to track validation dependencies. For a given channel our
// dependencies look like this: chanAnn <- chanUp <- nodeAnn. That is we must
// validate the item on the left of the arrow before that on the right.
type ValidationBarrier struct {
	// validationSemaphore is a channel of structs which is used as a
	// sempahore. Initially we'll fill this with a buffered channel of the
	// size of the number of active requests. Each new job will consume
	// from this channel, then restore the value upon completion.
	validationSemaphore chan struct{}

	// chanAnnFinSignal is map that keep track of all the pending
	// ChannelAnnouncement like validation job going on. Once the job has
	// been completed, the channel will be closed unblocking any
	// dependants.
	chanAnnFinSignal map[lnwire.ShortChannelID]chan struct{}

	// chanEdgeDependancies tracks any channel edge updates which should
	// wait until the completion of the ChannelAnnouncement before
	// proceeding. This is a dependency, as we can't validate the update
	// before we validate the announcement which creates the channel
	// itself.
	chanEdgeDependancies map[lnwire.ShortChannelID]chan struct{}

	// nodeAnnDependancies tracks any pending NodeAnnouncement validation
	// jobs which should wait until the completion of the
	// ChannelAnnouncement before proceeding.
	nodeAnnDependancies map[Vertex]chan struct{}

	quit chan struct{}
	sync.Mutex
}

// NewValidationBarrier creates a new instance of a validation barrier given
// the total number of active requests, and a quit channel which will be used
// to know when to kill an pending, but unfilled jobs.
func NewValidationBarrier(numActiveReqs int,
	quitChan chan struct{}) *ValidationBarrier {

	v := &ValidationBarrier{
		chanAnnFinSignal:     make(map[lnwire.ShortChannelID]chan struct{}),
		chanEdgeDependancies: make(map[lnwire.ShortChannelID]chan struct{}),
		nodeAnnDependancies:  make(map[Vertex]chan struct{}),
		quit:                 quitChan,
	}

	// We'll first initialize a set of sempahores to limit our concurrency
	// when validating incoming requests in parallel.
	v.validationSemaphore = make(chan struct{}, numActiveReqs)
	for i := 0; i < numActiveReqs; i++ {
		v.validationSemaphore <- struct{}{}
	}

	return v
}

// InitJobDependancies will wait for a new job slot to become open, and then
// sets up any dependant signals/trigger for the new job
func (v *ValidationBarrier) InitJobDependancies(job interface{}) {
	// We'll wait for either a new slot to become open, or for the quit
	// channel to be closed.
	select {
	case <-v.validationSemaphore:
	case <-v.quit:
	}

	v.Lock()
	defer v.Unlock()

	// Once a slot is open, we'll examine the message of the job, to see if
	// there need to be any dependant barriers set up.
	switch msg := job.(type) {

	// If this is a channel announcement, then we'll need to set up den
	// tenancies, as we'll need to verify this before we verify any
	// ChannelUpdates for the same channel, or NodeAnnouncements of nodes
	// that are involved in this channel. This goes for both the wire
	// type,s and also the types that we use within the database.
	case *lnwire.ChannelAnnouncement:

		// We ensure that we only create a new announcement signal iff,
		// one doesn't already exist, as there may be duplicate
		// announcements.  We'll close this signal once the
		// ChannelAnnouncement has been validated. This will result in
		// all the dependant jobs being unlocked so they can finish
		// execution themselves.
		if _, ok := v.chanAnnFinSignal[msg.ShortChannelID]; !ok {
			// We'll create the channel that we close after we
			// validate this announcement. All dependants will
			// point to this same channel, so they'll be unblocked
			// at the same time.
			annFinCond := make(chan struct{})
			v.chanAnnFinSignal[msg.ShortChannelID] = annFinCond
			v.chanEdgeDependancies[msg.ShortChannelID] = annFinCond

			v.nodeAnnDependancies[NewVertex(msg.NodeID1)] = annFinCond
			v.nodeAnnDependancies[NewVertex(msg.NodeID2)] = annFinCond
		}
	case *channeldb.ChannelEdgeInfo:

		shortID := lnwire.NewShortChanIDFromInt(msg.ChannelID)
		if _, ok := v.chanAnnFinSignal[shortID]; !ok {
			annFinCond := make(chan struct{})

			v.chanAnnFinSignal[shortID] = annFinCond
			v.chanEdgeDependancies[shortID] = annFinCond

			v.nodeAnnDependancies[NewVertex(msg.NodeKey1)] = annFinCond
			v.nodeAnnDependancies[NewVertex(msg.NodeKey2)] = annFinCond
		}

	// These other types don't have any dependants, so no further
	// initialization needs to be done beyond just occupying a job slot.
	case *channeldb.ChannelEdgePolicy:
		return
	case *lnwire.ChannelUpdate:
		return
	case *lnwire.NodeAnnouncement:
		return
	case *channeldb.LightningNode:
		return
	case *lnwire.AnnounceSignatures:
		// TODO(roasbeef): need to wait on chan ann?
		return
	}
}

// CompleteJob returns a free slot to the set of available job slots. This
// should be called once a job has been fully completed. Otherwise, slots may
// not be returned to the internal scheduling, causing a deadlock when a new
// overflow job is attempted.
func (v *ValidationBarrier) CompleteJob() {
	select {
	case v.validationSemaphore <- struct{}{}:
	case <-v.quit:
	}
}

// WaitForDependants will block until any jobs that this job dependants on have
// finished executing. This allows us a graceful way to schedule goroutines
// based on any pending uncompleted dependant jobs. If this job doesn't have an
// active dependant, then this function will return immediately.
func (v *ValidationBarrier) WaitForDependants(job interface{}) {

	var (
		signal chan struct{}
		ok     bool
	)

	v.Lock()
	switch msg := job.(type) {

	// Any ChannelUpdate or NodeAnnouncement jobs will need to wait on the
	// completion of any active ChannelAnnouncement jobs related to them.
	case *channeldb.ChannelEdgePolicy:
		shortID := lnwire.NewShortChanIDFromInt(msg.ChannelID)
		signal, ok = v.chanEdgeDependancies[shortID]
	case *channeldb.LightningNode:
		vertex := NewVertex(msg.PubKey)
		signal, ok = v.nodeAnnDependancies[vertex]
	case *lnwire.ChannelUpdate:
		signal, ok = v.chanEdgeDependancies[msg.ShortChannelID]
	case *lnwire.NodeAnnouncement:
		vertex := NewVertex(msg.NodeID)
		signal, ok = v.nodeAnnDependancies[vertex]

	// Other types of jobs can be executed immediately, so we'll just
	// return directly.
	case *lnwire.AnnounceSignatures:
		// TODO(roasbeef): need to wait on chan ann?
		v.Unlock()
		return
	case *channeldb.ChannelEdgeInfo:
		v.Unlock()
		return
	case *lnwire.ChannelAnnouncement:
		v.Unlock()
		return
	}
	v.Unlock()

	// If we do have an active job, then we'll wait until either the signal
	// is closed, or the set of jobs exits.
	if ok {
		select {
		case <-v.quit:
			return
		case <-signal:
		}
	}
}

// SignalDependants will signal any jobs that are dependant on this job that
// they can continue execution. If the job doesn't have any dependants, then
// this function sill exit immediately.
func (v *ValidationBarrier) SignalDependants(job interface{}) {
	v.Lock()
	defer v.Unlock()

	switch msg := job.(type) {

	// If we've just finished executing a ChannelAnnouncement, then we'll
	// close out the signal, and remove the signal from the map of active
	// ones. This will allow any dependant jobs to continue execution.
	case *channeldb.ChannelEdgeInfo:
		shortID := lnwire.NewShortChanIDFromInt(msg.ChannelID)
		finSignal, ok := v.chanAnnFinSignal[shortID]
		if ok {
			close(finSignal)
			delete(v.chanAnnFinSignal, shortID)
		}
	case *lnwire.ChannelAnnouncement:
		finSignal, ok := v.chanAnnFinSignal[msg.ShortChannelID]
		if ok {
			close(finSignal)
			delete(v.chanAnnFinSignal, msg.ShortChannelID)
		}

		delete(v.chanEdgeDependancies, msg.ShortChannelID)

	// For all other job types, we'll delete the tracking entries from the
	// map, as if we reach this point, then all dependants have already
	// finished executing and we can proceed.
	case *channeldb.LightningNode:
		delete(v.nodeAnnDependancies, NewVertex(msg.PubKey))
	case *lnwire.NodeAnnouncement:
		delete(v.nodeAnnDependancies, NewVertex(msg.NodeID))
	case *lnwire.ChannelUpdate:
		delete(v.chanEdgeDependancies, msg.ShortChannelID)
	case *channeldb.ChannelEdgePolicy:
		shortID := lnwire.NewShortChanIDFromInt(msg.ChannelID)
		delete(v.chanEdgeDependancies, shortID)

	case *lnwire.AnnounceSignatures:
		return
	}
}