package dav

import (
	"encoding/xml"
	"fmt"
)

// PropPatch collects property mutations from a PROPPATCH request and lets
// nodes/plugins claim the ones they can apply. Port of sabre's PropPatch,
// including its status-code protocol: claimed properties are 202 (pending)
// until their handler runs; unclaimed ones fail with 403; any failure (>=400)
// turns every pending/successful property into 424 Failed Dependency.
type PropPatch struct {
	// mutations maps property name to the new value: string for text
	// content, []byte for a raw XML fragment, nil for removal.
	mutations map[xml.Name]any
	result    map[xml.Name]int
	callbacks []patchCallback
	failed    bool
}

// PatchHandler applies a set of mutations. It returns per-property status
// codes, or nil and ok to signal uniform success (200/204) or failure (403)
// for all values, mirroring sabre's bool-or-array callback returns.
type PatchHandler func(values map[xml.Name]any) (statuses map[xml.Name]int, ok bool)

type patchCallback struct {
	names []xml.Name
	fn    PatchHandler
}

// NewPropPatch creates a PropPatch for the given mutations.
func NewPropPatch(mutations map[xml.Name]any) *PropPatch {
	return &PropPatch{
		mutations: mutations,
		result:    make(map[xml.Name]int, len(mutations)),
	}
}

// Handle registers a callback for the subset of names present in the
// mutations and not yet claimed.
func (pp *PropPatch) Handle(names []xml.Name, fn PatchHandler) {
	var used []xml.Name
	for _, n := range names {
		if _, mutated := pp.mutations[n]; mutated {
			if _, claimed := pp.result[n]; !claimed {
				used = append(used, n)
				pp.result[n] = 202
			}
		}
	}
	if len(used) > 0 {
		pp.callbacks = append(pp.callbacks, patchCallback{used, fn})
	}
}

// HandleRemaining registers a catch-all callback for every unclaimed mutation.
func (pp *PropPatch) HandleRemaining(fn PatchHandler) {
	remaining := pp.Remaining()
	if len(remaining) == 0 {
		return
	}
	for _, n := range remaining {
		pp.result[n] = 202
	}
	pp.callbacks = append(pp.callbacks, patchCallback{remaining, fn})
}

// Mutation returns the requested change for name: the new value (string
// for text content, []byte for a raw XML fragment, nil for removal) and
// whether the request changes name at all. Lets a pre-hook validate a value
// before claiming it.
func (pp *PropPatch) Mutation(name xml.Name) (value any, ok bool) {
	value, ok = pp.mutations[name]
	return value, ok
}

// Remaining lists mutations no handler has claimed yet.
func (pp *PropPatch) Remaining() []xml.Name {
	var out []xml.Name
	for n := range pp.mutations {
		if _, claimed := pp.result[n]; !claimed {
			out = append(out, n)
		}
	}
	return out
}

// SetResultCode forces a status for the given properties (used by plugins
// that reject mutations outright).
func (pp *PropPatch) SetResultCode(names []xml.Name, status int) {
	for _, n := range names {
		pp.result[n] = status
	}
	if status >= 400 {
		pp.failed = true
	}
}

// Commit runs all handlers and returns the final per-property status map and
// overall success.
func (pp *PropPatch) Commit() (map[xml.Name]int, bool) {
	for n := range pp.mutations {
		if _, claimed := pp.result[n]; !claimed {
			pp.failed = true
			pp.result[n] = 403
		}
	}

	for _, cb := range pp.callbacks {
		if pp.failed {
			break
		}
		pp.runCallback(cb)
	}

	if pp.failed {
		for n, status := range pp.result {
			if status == 202 {
				pp.result[n] = 424
			}
		}
	}
	return pp.result, !pp.failed
}

func (pp *PropPatch) runCallback(cb patchCallback) {
	values := make(map[xml.Name]any, len(cb.names))
	for _, n := range cb.names {
		values[n] = pp.mutations[n]
	}
	statuses, ok := cb.fn(values)

	if statuses != nil {
		for _, n := range cb.names {
			status, present := statuses[n]
			if !present {
				status = 500
			}
			if status >= 400 {
				pp.failed = true
			}
			pp.result[n] = status
		}
		return
	}
	for _, n := range cb.names {
		switch {
		case !ok:
			pp.result[n] = 403
			pp.failed = true
		case pp.mutations[n] == nil:
			pp.result[n] = 204 // deletion
		default:
			pp.result[n] = 200 // update
		}
	}
}

func (pp *PropPatch) String() string {
	return fmt.Sprintf("PropPatch(%d mutations)", len(pp.mutations))
}
