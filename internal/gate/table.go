package gate

import (
	"fmt"
	"io"
	"strings"
	"text/tabwriter"
)

var icon = map[Status]string{Pass: "✅", Fail: "❌", Pending: "⏳", Skip: "➖"}

// Markdown renders the evidence table for a PR body or commit comment.
func (r *Report) Markdown() string {
	var b strings.Builder
	fmt.Fprintf(&b, "**%s** in **%s** on `%s`, chart `%s` (`%s`)\n\n", r.App, r.Tier, r.Cluster, r.Version, r.Digest)
	b.WriteString("| check | result | value | query / reason |\n|---|---|---|---|\n")
	for _, x := range r.Results {
		detail := x.Query
		if x.Reason != "" {
			if detail != "" {
				detail = x.Reason + " — `" + detail + "`"
			} else {
				detail = x.Reason
			}
		} else if detail != "" {
			detail = "`" + detail + "`"
		}
		fmt.Fprintf(&b, "| %s | %s %s | %s | %s |\n", md(x.Check), icon[x.Status], x.Status, md(x.Value), md(detail))
	}
	fmt.Fprintf(&b, "\nGate ran %s → %s (%s).\n", r.Started.UTC().Format("2006-01-02 15:04:05Z"),
		r.Finished.UTC().Format("15:04:05Z"), r.Finished.Sub(r.Started).Round(1e9))
	return b.String()
}

// Text writes the table for a terminal.
func (r *Report) Text(w io.Writer) {
	fmt.Fprintf(w, "%s / %s / %s  chart %s %s\n", r.App, r.Tier, r.Cluster, r.Version, r.Digest)
	tw := tabwriter.NewWriter(w, 0, 0, 2, ' ', 0)
	fmt.Fprintln(tw, "CHECK\tRESULT\tVALUE\tQUERY / REASON")
	for _, x := range r.Results {
		detail := x.Query
		if x.Reason != "" {
			detail = x.Reason
			if x.Query != "" {
				detail += "  [" + x.Query + "]"
			}
		}
		fmt.Fprintf(tw, "%s\t%s %s\t%s\t%s\n", x.Check, icon[x.Status], x.Status, x.Value, detail)
	}
	tw.Flush()
	if r.Passed() {
		fmt.Fprintln(w, "GATE PASSED")
	} else {
		fmt.Fprintln(w, "GATE FAILED: "+r.Summary())
	}
}

func md(s string) string {
	s = strings.ReplaceAll(s, "|", `\|`)
	return strings.ReplaceAll(s, "\n", " ")
}
