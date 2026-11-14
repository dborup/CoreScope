package dhm

// Admission bounds how many distinct upstream calls may exist at once —
// running or queued for a concurrency slot (#744). Flight takes one slot
// before it starts a call's worker and gives it back when the worker has
// returned, so a refused call costs no goroutine at all. Callers that
// join a call already running take no slot: they add no upstream work.
//
// A nil *Admission admits everything.
type Admission struct {
	slots chan struct{}
}

// NewAdmission returns an Admission of n slots; n < 1 is 1.
func NewAdmission(n int) *Admission {
	if n < 1 {
		n = 1
	}
	return &Admission{slots: make(chan struct{}, n)}
}

// ErrAdmissionFull is returned, without any upstream work, when every
// Admission slot is taken.
var ErrAdmissionFull = FetchError{"admission full"}

func (a *Admission) tryAcquire() bool {
	if a == nil {
		return true
	}
	select {
	case a.slots <- struct{}{}:
		return true
	default:
		return false
	}
}

func (a *Admission) release() {
	if a != nil {
		<-a.slots
	}
}

// InUse is the number of slots taken.
func (a *Admission) InUse() int {
	if a == nil {
		return 0
	}
	return len(a.slots)
}

// Cap is the number of slots.
func (a *Admission) Cap() int {
	if a == nil {
		return 0
	}
	return cap(a.slots)
}
