package replay

import (
	"fmt"
	"io"
)

// Result is one named check -- a scenario or a final assertion -- and
// whether it held.
type Result struct {
	Name   string
	Passed bool
	Detail string
}

// Report is everything one Run produced, printed for a human and
// checked by Passed() for CI.
type Report struct {
	Seed       int64
	Scenarios  []Result
	Assertions []Result
}

// Passed reports whether every scenario and every final assertion held.
func (r *Report) Passed() bool {
	for _, s := range r.Scenarios {
		if !s.Passed {
			return false
		}
	}
	for _, a := range r.Assertions {
		if !a.Passed {
			return false
		}
	}
	return true
}

// Print writes a human-readable summary, ending with the seed needed to
// reproduce a failure -- same posture as every prior component's own
// replay report.
func (r *Report) Print(w io.Writer) {
	fmt.Fprintf(w, "relayd replay harness -- seed %d\n\nScenarios:\n", r.Seed)
	for _, s := range r.Scenarios {
		printResult(w, s)
	}
	fmt.Fprintln(w, "\nFinal assertions:")
	for _, a := range r.Assertions {
		printResult(w, a)
	}
	if r.Passed() {
		fmt.Fprintln(w, "\nALL PASSED")
	} else {
		fmt.Fprintf(w, "\nFAILED -- reproduce with -seed %d\n", r.Seed)
	}
}

func printResult(w io.Writer, r Result) {
	status := "PASS"
	if !r.Passed {
		status = "FAIL"
	}
	if r.Detail != "" {
		fmt.Fprintf(w, "  [%s] %s -- %s\n", status, r.Name, r.Detail)
		return
	}
	fmt.Fprintf(w, "  [%s] %s\n", status, r.Name)
}

func pass(name string) Result { return Result{Name: name, Passed: true} }
func fail(name string, err error) Result {
	return Result{Name: name, Passed: false, Detail: err.Error()}
}
