package plan

import (
	"encoding/json"
	"fmt"
	"strings"
)

// String renders the op as it goes on the wire: verb, path, JSON body. JSON
// object keys are sorted (encoding/json sorts map keys), so the rendering is
// deterministic.
func (op Op) String() string {
	s := fmt.Sprintf("%s %s", op.Method, op.Path)
	if op.Body != nil {
		body, err := json.Marshal(op.Body)
		if err != nil {
			body = []byte(fmt.Sprintf("<unencodable body: %v>", err))
		}
		s += " " + string(body)
	}
	return s
}

// Render is the dry-run text of the plan (project.md §5.4: "Dry run lists
// every REST operation ... before anything is written"): the numbered ops
// grouped by target router, each with its explanatory note, followed by any
// warnings and then any skipped hunks. It is what the Apply screen shows and what the planner's
// golden tests pin.
func (p Plan) Render() string {
	var b strings.Builder
	if p.Empty() {
		b.WriteString("no operations\n")
	}

	current := ""
	for i, op := range p.Ops {
		if op.Router != current {
			current = op.Router
			fmt.Fprintf(&b, "router %s:\n", strings.ToUpper(current))
		}
		fmt.Fprintf(&b, "%3d. %s\n", i+1, op)
		if op.Note != "" {
			fmt.Fprintf(&b, "       %s\n", op.Note)
		}
	}

	if len(p.Warnings) > 0 {
		b.WriteString("warnings:\n")
		for _, w := range p.Warnings {
			fmt.Fprintf(&b, "  %s\n", w)
		}
	}

	if len(p.Skipped) > 0 {
		b.WriteString("skipped:\n")
		for _, s := range p.Skipped {
			fmt.Fprintf(&b, "  %s: %s\n", s.Where(), s.Reason)
		}
	}
	return b.String()
}
