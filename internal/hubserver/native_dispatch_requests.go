package hubserver

// nativeDispatchState is the pair a claim decision is made against: the
// version of the item an attempt answers, and the generation of the last
// explicit request for a new attempt on it.
type nativeDispatchState struct {
	revision   int64
	generation int64
}
