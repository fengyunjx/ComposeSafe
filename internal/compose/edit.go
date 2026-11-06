package compose

import (
	"bytes"
	"errors"
	"fmt"
	"os"
	"regexp"
	"strings"
	"unicode/utf8"

	"gopkg.in/yaml.v3"
)

// This file edits compose files as TEXT. The YAML parser is used only to
// find where a value sits (line and column); the bytes around it are never
// re-encoded, so comments, indentation, anchors and blank lines stay exactly
// as the user wrote them. A re-serialising approach would reformat the whole
// file, and a diff in the user's compose file is the first thing they would
// notice (DESIGN 5.2, 8.3).

// ManualEditError means the change cannot be made safely by a program; the
// user has to make it. Reason says why.
type ManualEditError struct{ Reason string }

func (e *ManualEditError) Error() string {
	return "cannot edit the compose file automatically: " + e.Reason
}

func manual(format string, a ...any) error {
	return &ManualEditError{Reason: fmt.Sprintf(format, a...)}
}

// FileEdit is a pending change of one file.
type FileEdit struct {
	Path          string
	Before, After []byte
	Summary       string // one line for the user
}

// ApplyEdits writes the edits atomically (temporary file, then rename),
// keeping each file's mode. It returns an undo function that puts the
// original bytes back; it is safe to call even when ApplyEdits failed midway.
func ApplyEdits(edits []FileEdit) (undo func() error, err error) {
	var done []FileEdit
	undo = func() error {
		var errs []error
		for i := len(done) - 1; i >= 0; i-- {
			errs = append(errs, writeAtomic(done[i].Path, done[i].Before))
		}
		return errors.Join(errs...)
	}
	for _, e := range edits {
		if err := writeAtomic(e.Path, e.After); err != nil {
			return undo, fmt.Errorf("write %s: %w", e.Path, err)
		}
		done = append(done, e)
	}
	return undo, nil
}

func writeAtomic(path string, data []byte) error {
	mode := os.FileMode(0o644)
	if fi, err := os.Stat(path); err == nil {
		mode = fi.Mode().Perm()
	}
	tmp := path + ".composesafe-tmp"
	if err := os.WriteFile(tmp, data, mode); err != nil {
		return err
	}
	if err := os.Chmod(tmp, mode); err != nil {
		_ = os.Remove(tmp)
		return err
	}
	return os.Rename(tmp, path)
}

// ---- locating nodes ----------------------------------------------------------

func parseDoc(src []byte) (*yaml.Node, error) {
	var doc yaml.Node
	if err := yaml.Unmarshal(src, &doc); err != nil {
		return nil, err
	}
	if doc.Kind != yaml.DocumentNode || len(doc.Content) == 0 {
		return nil, errors.New("empty document")
	}
	return doc.Content[0], nil
}

// mapGet returns the value node for key in a mapping node.
func mapGet(m *yaml.Node, key string) (k, v *yaml.Node) {
	if m == nil || m.Kind != yaml.MappingNode {
		return nil, nil
	}
	for i := 0; i+1 < len(m.Content); i += 2 {
		if m.Content[i].Value == key {
			return m.Content[i], m.Content[i+1]
		}
	}
	return nil, nil
}

// serviceNode returns services.<name> of a parsed document, or nil.
func serviceNode(root *yaml.Node, name string) *yaml.Node {
	_, services := mapGet(root, "services")
	_, svc := mapGet(services, name)
	if svc != nil && svc.Kind == yaml.AliasNode {
		return svc.Alias
	}
	return svc
}

// offsets maps (line, column) of the parser to byte offsets of src.
type offsets struct {
	src    []byte
	starts []int
}

func newOffsets(src []byte) *offsets {
	o := &offsets{src: src, starts: []int{0}}
	for i, b := range src {
		if b == '\n' {
			o.starts = append(o.starts, i+1)
		}
	}
	return o
}

// at returns the byte offset of a node's start. yaml.v3 columns count
// characters (runes), not bytes.
func (o *offsets) at(n *yaml.Node) (int, error) {
	if n.Line < 1 || n.Line > len(o.starts) {
		return 0, manual("cannot locate line %d", n.Line)
	}
	off := o.starts[n.Line-1]
	for c := 1; c < n.Column; c++ {
		if off >= len(o.src) {
			return 0, manual("cannot locate column %d on line %d", n.Column, n.Line)
		}
		_, sz := utf8.DecodeRune(o.src[off:])
		off += sz
	}
	return off, nil
}

// scalarSpan returns the byte range of the text of a single-line scalar,
// without its quotes.
func (o *offsets) scalarSpan(n *yaml.Node) (start, end int, err error) {
	if n.Kind != yaml.ScalarNode {
		return 0, 0, manual("line %d is not a plain value", n.Line)
	}
	if n.Style&(yaml.LiteralStyle|yaml.FoldedStyle) != 0 {
		return 0, 0, manual("line %d is a multi-line value", n.Line)
	}
	off, err := o.at(n)
	if err != nil {
		return 0, 0, err
	}
	switch {
	case n.Style&yaml.DoubleQuotedStyle != 0:
		start = off + 1
		for i := start; i < len(o.src) && o.src[i] != '\n'; i++ {
			if o.src[i] == '\\' {
				i++
				continue
			}
			if o.src[i] == '"' {
				return start, i, nil
			}
		}
	case n.Style&yaml.SingleQuotedStyle != 0:
		start = off + 1
		for i := start; i < len(o.src) && o.src[i] != '\n'; i++ {
			if o.src[i] == '\'' {
				if i+1 < len(o.src) && o.src[i+1] == '\'' {
					i++
					continue
				}
				return start, i, nil
			}
		}
	default:
		end = off + len(n.Value)
		if end <= len(o.src) && string(o.src[off:end]) == n.Value {
			return off, end, nil
		}
	}
	return 0, 0, manual("cannot find the text of the value on line %d", n.Line)
}

// replace returns src with [start,end) replaced.
func replace(src []byte, start, end int, text string) []byte {
	out := make([]byte, 0, len(src)-(end-start)+len(text))
	out = append(out, src[:start]...)
	out = append(out, text...)
	return append(out, src[end:]...)
}

// safeText limits what we write into a YAML scalar without quoting.
var safeText = regexp.MustCompile(`^[A-Za-z0-9._/@:+\-${}]+$`)

// ---- image ---------------------------------------------------------------------

var varRef = regexp.MustCompile(`\$\{([A-Za-z_][A-Za-z0-9_]*)(?::?-([^}]*))?\}`)

// EditImage computes the edits that change the image of a service from
// oldImage to newImage (both fully resolved, as `docker compose config`
// reports them). files are the compose files in merge order; envFiles are
// the dotenv files.
//
// The image is changed where it is written: in the last compose file that
// defines it, or, when it is `repo:${VAR}`, in the .env line that sets VAR.
// Anything else (anchors, extends, include, several variables, a variable
// that is not in .env) returns a *ManualEditError.
func EditImage(files, envFiles []string, service, oldImage, newImage string) ([]FileEdit, error) {
	if !safeText.MatchString(newImage) || strings.Contains(newImage, "$") {
		return nil, manual("the new image reference %q has unexpected characters", newImage)
	}
	type found struct {
		path string
		src  []byte
		node *yaml.Node
	}
	var win *found
	sawIndirect := false
	for _, f := range files {
		src, err := os.ReadFile(f)
		if err != nil {
			return nil, err
		}
		root, err := parseDoc(src)
		if err != nil {
			return nil, fmt.Errorf("%s: %w", f, err)
		}
		svc := serviceNode(root, service)
		if svc == nil {
			if k, _ := mapGet(root, "include"); k != nil {
				sawIndirect = true
			}
			continue
		}
		if _, ext := mapGet(svc, "extends"); ext != nil {
			sawIndirect = true
		}
		for i := 0; i+1 < len(svc.Content); i += 2 {
			if svc.Content[i].Value == "<<" {
				sawIndirect = true
			}
		}
		if _, img := mapGet(svc, "image"); img != nil {
			if img.Kind == yaml.AliasNode {
				return nil, manual("the image of %q is an alias (*%s) to a shared value: change it where it is defined", service, img.Value)
			}
			win = &found{f, src, img}
		}
	}
	if win == nil {
		if sawIndirect {
			return nil, manual("the image of %q is inherited (anchor, merge key, extends or include): change it where it is defined", service)
		}
		return nil, manual("no compose file sets an image for %q", service)
	}

	raw := win.node.Value
	if !strings.Contains(raw, "$") {
		if raw != oldImage {
			return nil, manual("%s sets %q but Compose resolves %q (an override file or the environment wins): change it where it takes effect", win.path, raw, oldImage)
		}
		start, end, err := newOffsets(win.src).scalarSpan(win.node)
		if err != nil {
			return nil, err
		}
		after := replace(win.src, start, end, newImage)
		if err := checkImage(after, service, newImage); err != nil {
			return nil, err
		}
		return []FileEdit{{Path: win.path, Before: win.src, After: after,
			Summary: fmt.Sprintf("%s: services.%s.image %s → %s", win.path, service, oldImage, newImage)}}, nil
	}

	// repo:${VAR} — change the variable, not the compose file.
	locs := varRef.FindAllStringSubmatchIndex(raw, -1)
	if len(locs) != 1 || strings.Count(raw, "$") != 1 {
		return nil, manual("the image %q uses variables in a way that cannot be changed automatically", raw)
	}
	l := locs[0]
	name := raw[l[2]:l[3]]
	prefix, suffix := raw[:l[0]], raw[l[1]:]
	if !strings.HasPrefix(oldImage, prefix) || !strings.HasSuffix(oldImage, suffix) ||
		!strings.HasPrefix(newImage, prefix) || !strings.HasSuffix(newImage, suffix) ||
		len(newImage) < len(prefix)+len(suffix) {
		return nil, manual("the new image %q does not fit the pattern %q", newImage, raw)
	}
	oldVal := oldImage[len(prefix) : len(oldImage)-len(suffix)]
	newVal := newImage[len(prefix) : len(newImage)-len(suffix)]

	var hit *FileEdit
	for _, f := range envFiles {
		src, err := os.ReadFile(f)
		if err != nil {
			return nil, err
		}
		after, cur, ok := SetEnvValue(src, name, newVal)
		if !ok {
			continue
		}
		if cur != oldVal {
			return nil, manual("%s sets %s=%q but the running image uses %q (the environment overrides the file)", f, name, cur, oldVal)
		}
		hit = &FileEdit{Path: f, Before: src, After: after,
			Summary: fmt.Sprintf("%s: %s=%s → %s", f, name, oldVal, newVal)}
	}
	if hit == nil {
		return nil, manual("the image uses ${%s}, which is not set in a .env file (it comes from the environment or a default): change it there", name)
	}
	return []FileEdit{*hit}, nil
}

// checkImage re-parses the edited file and confirms the image is what we meant.
func checkImage(src []byte, service, want string) error {
	root, err := parseDoc(src)
	if err != nil {
		return fmt.Errorf("the edited file does not parse: %w", err)
	}
	_, img := mapGet(serviceNode(root, service), "image")
	if img == nil || img.Value != want {
		return manual("internal check failed: the edited image of %q is not %q", service, want)
	}
	return nil
}

// SetEnvValue changes the value of NAME in dotenv content, leaving every
// other byte alone (comments, order, quoting style of the value). It returns
// the new content, the previous value and whether NAME was found. When NAME
// is set more than once the last line wins, as in Compose.
func SetEnvValue(src []byte, name, value string) (out []byte, old string, found bool) {
	lines := bytes.SplitAfter(src, []byte("\n"))
	hit := -1
	for i, ln := range lines {
		t := strings.TrimSpace(string(ln))
		t = strings.TrimPrefix(t, "export ")
		if k, _, ok := strings.Cut(t, "="); ok && strings.TrimSpace(k) == name && !strings.HasPrefix(strings.TrimSpace(string(ln)), "#") {
			hit = i
		}
	}
	if hit < 0 {
		return src, "", false
	}
	ln := string(lines[hit])
	eq := strings.Index(ln, "=")
	rest := ln[eq+1:]
	nl := ""
	if strings.HasSuffix(rest, "\r\n") {
		nl, rest = "\r\n", strings.TrimSuffix(rest, "\r\n")
	} else if strings.HasSuffix(rest, "\n") {
		nl, rest = "\n", strings.TrimSuffix(rest, "\n")
	}
	trimmed := strings.TrimSpace(rest)
	quote := ""
	if len(trimmed) >= 2 && (trimmed[0] == '"' || trimmed[0] == '\'') && trimmed[len(trimmed)-1] == trimmed[0] {
		quote = string(trimmed[0])
		old = trimmed[1 : len(trimmed)-1]
	} else {
		old = trimmed
		if i := strings.Index(old, " #"); i >= 0 { // inline comment on an unquoted value
			old = strings.TrimSpace(old[:i])
		}
	}
	lines[hit] = []byte(ln[:eq+1] + quote + value + quote + nl)
	return bytes.Join(lines, nil), old, true
}

// ---- volumes -------------------------------------------------------------------

// SwitchMount changes the source of the mount that targets `target` in a
// service (a named volume key, or a bind path) and returns the old source
// text. newSource receives the old text and whether it is a bind path.
// Short ("src:target[:mode]") and long (mapping) syntax are supported.
func SwitchMount(src []byte, service, target string, newSource func(old string, bind bool) string) (out []byte, old string, bind bool, err error) {
	return SwitchMountTo(src, service, target, "", newSource)
}

// SwitchMountTo is SwitchMount that can also move the mount point inside the
// container: when newTarget is not empty the mount's target path is replaced
// as well (Postgres 18 keeps its data under /var/lib/postgresql).
func SwitchMountTo(src []byte, service, target, newTarget string, newSource func(old string, bind bool) string) (out []byte, old string, bind bool, err error) {
	if newTarget != "" && !safeText.MatchString(newTarget) {
		return nil, "", false, manual("the new mount target %q has unexpected characters", newTarget)
	}
	root, err := parseDoc(src)
	if err != nil {
		return nil, "", false, err
	}
	svc := serviceNode(root, service)
	if svc == nil {
		return nil, "", false, manual("service %q is not defined in this file", service)
	}
	_, vols := mapGet(svc, "volumes")
	if vols == nil || vols.Kind != yaml.SequenceNode {
		return nil, "", false, manual("service %q has no volumes list in this file", service)
	}
	offs := newOffsets(src)
	for _, e := range vols.Content {
		switch e.Kind {
		case yaml.ScalarNode: // short syntax
			parts := strings.Split(e.Value, ":")
			if len(parts) < 2 || parts[1] != target {
				continue
			}
			s, _, err := offs.scalarSpan(e)
			if err != nil {
				return nil, "", false, err
			}
			old, bind = parts[0], isBindPath(parts[0])
			repl := newSource(old, bind)
			if !safeText.MatchString(repl) || strings.Contains(repl, "$") && !strings.Contains(old, "$") {
				return nil, "", false, manual("the new mount source %q has unexpected characters", repl)
			}
			if newTarget == "" {
				return replace(src, s, s+len(old), repl), old, bind, nil
			}
			// "old:target" becomes "repl:newTarget"; the mode suffix is untouched.
			return replace(src, s, s+len(old)+1+len(parts[1]), repl+":"+newTarget), old, bind, nil
		case yaml.MappingNode: // long syntax
			_, t := mapGet(e, "target")
			_, sv := mapGet(e, "source")
			if t == nil || t.Value != target || sv == nil {
				continue
			}
			_, typ := mapGet(e, "type")
			old = sv.Value
			bind = (typ != nil && typ.Value == "bind") || isBindPath(old)
			s, en, err := offs.scalarSpan(sv)
			if err != nil {
				return nil, "", false, err
			}
			repl := newSource(old, bind)
			if !safeText.MatchString(repl) {
				return nil, "", false, manual("the new mount source %q has unexpected characters", repl)
			}
			out = replace(src, s, en, repl)
			if newTarget == "" {
				return out, old, bind, nil
			}
			ts, te, err := offs.scalarSpan(t)
			if err != nil {
				return nil, "", false, err
			}
			// Both spans refer to the original text: change the later one first.
			if ts > s {
				out = replace(replace(src, ts, te, newTarget), s, en, repl)
			} else {
				out = replace(replace(src, s, en, repl), ts, te, newTarget)
			}
			return out, old, bind, nil
		}
	}
	return nil, "", false, manual("no mount at %s found for %q in this file", target, service)
}

// isBindPath: Compose treats a short-syntax source as a path when it starts
// with ".", "/" or "~"; anything else is a named volume.
func isBindPath(s string) bool {
	return strings.HasPrefix(s, ".") || strings.HasPrefix(s, "/") || strings.HasPrefix(s, "~")
}

// DeclareExternalVolume adds
//
//	<key>:
//	  external: true
//	  name: <name>
//
// to the top-level `volumes:` mapping (creating the section when missing).
// The volume must already exist: Compose will not create or label it.
func DeclareExternalVolume(src []byte, key, name string) ([]byte, error) {
	if !safeText.MatchString(key) || !safeText.MatchString(name) {
		return nil, manual("volume name %q has unexpected characters", key)
	}
	root, err := parseDoc(src)
	if err != nil {
		return nil, err
	}
	_, vols := mapGet(root, "volumes")
	nl := "\n"
	if bytes.Contains(src, []byte("\r\n")) {
		nl = "\r\n"
	}
	body := func(indent string) string {
		return fmt.Sprintf("%s%s:%s%s  external: true%s%s  name: %s%s", indent, key, nl, indent, nl, indent, name, nl)
	}
	appendAtEnd := func(prefix string) []byte {
		out := append([]byte(nil), src...)
		if len(out) > 0 && out[len(out)-1] != '\n' {
			out = append(out, nl...)
		}
		return append(out, []byte(prefix)...)
	}

	switch {
	case vols == nil:
		return appendAtEnd("volumes:" + nl + body("  ")), nil
	case vols.Kind == yaml.MappingNode && vols.Style&yaml.FlowStyle != 0:
		return nil, manual("the top-level volumes section is written in {flow} style")
	case vols.Kind == yaml.MappingNode && len(vols.Content) > 0:
		indent := strings.Repeat(" ", vols.Content[0].Column-1)
		last := lastLine(vols)
		offs := newOffsets(src)
		if last >= len(offs.starts) { // last line is the final one of the file
			return append(appendAtEnd(""), []byte(body(indent))...), nil
		}
		at := offs.starts[last] // start of the line after the section
		return replace(src, at, at, body(indent)), nil
	default: // "volumes:" with nothing under it
		k, _ := mapGet(root, "volumes")
		offs := newOffsets(src)
		if k.Line >= len(offs.starts) {
			return append(appendAtEnd(""), []byte(body("  "))...), nil
		}
		at := offs.starts[k.Line]
		// the empty value "volumes:" may carry "{}" or null; only the null form is handled
		if vols.Kind == yaml.ScalarNode && vols.Value != "" && vols.Tag != "!!null" {
			return nil, manual("the top-level volumes section has an unexpected value")
		}
		return replace(src, at, at, body("  ")), nil
	}
}

// lastLine returns the largest line number used by a node and its children.
func lastLine(n *yaml.Node) int {
	m := n.Line
	for _, c := range n.Content {
		if l := lastLine(c); l > m {
			m = l
		}
	}
	return m
}

// VolumeDeclared reports whether the top-level volumes section declares key.
func VolumeDeclared(src []byte, key string) bool {
	root, err := parseDoc(src)
	if err != nil {
		return false
	}
	_, vols := mapGet(root, "volumes")
	k, _ := mapGet(vols, key)
	return k != nil
}
