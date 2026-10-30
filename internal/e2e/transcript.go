package e2e

import (
	"fmt"
	"regexp"
	"strings"
)

// Block is one top-level piece of a transcript: prose lines, or a command with its expected output.
type Block struct {
	// Prose holds unindented lines (blank lines included) when Cmd is empty and IsCmd is false.
	Prose []string
	// IsCmd marks a command block.
	IsCmd bool
	// Cmd is the command line after "$ ".
	Cmd string
	// Out is the expected output, without the two-space indent. Elision lines ("...", "~ RE")
	// stay as written.
	Out []string
}

// File is a "-- path --" section.
type File struct {
	Path  string
	Lines []string
}

// Transcript is a parsed journey file.
type Transcript struct {
	Blocks []Block
	Files  []File
}

var fileMarker = regexp.MustCompile(`^-- (\S.*?) --$`)

const (
	indent    = "  "
	cmdPrefix = "$ "
)

// Parse reads a journey file. It fails on an indented line that follows no command.
func Parse(text string) (*Transcript, error) {
	text = strings.ReplaceAll(text, "\r", "") // CR never survives a round trip
	lines := strings.Split(strings.TrimSuffix(text, "\n"), "\n")
	if text == "" {
		lines = nil
	}
	t := &Transcript{}
	var cur *Block
	flushProse := func() *Block {
		if n := len(t.Blocks); n > 0 && !t.Blocks[n-1].IsCmd {
			return &t.Blocks[n-1]
		}
		t.Blocks = append(t.Blocks, Block{})
		return &t.Blocks[len(t.Blocks)-1]
	}
	for i := 0; i < len(lines); i++ {
		ln := lines[i]
		if m := fileMarker.FindStringSubmatch(ln); m != nil {
			f := File{Path: m[1]}
			for i++; i < len(lines) && !fileMarker.MatchString(lines[i]); i++ {
				f.Lines = append(f.Lines, lines[i])
			}
			i--
			t.Files = append(t.Files, f)
			cur = nil
			continue
		}
		if len(t.Files) > 0 {
			return nil, fmt.Errorf("line %d: content after a file section", i+1)
		}
		switch {
		case strings.HasPrefix(ln, indent+cmdPrefix):
			t.Blocks = append(t.Blocks, Block{IsCmd: true, Cmd: ln[len(indent)+len(cmdPrefix):]})
			cur = &t.Blocks[len(t.Blocks)-1]
		case strings.HasPrefix(ln, indent):
			if cur == nil {
				return nil, fmt.Errorf("line %d: indented line outside a command's output", i+1)
			}
			cur.Out = append(cur.Out, ln[len(indent):])
		case ln == "" && cur != nil && continuesOutput(lines, i):
			cur.Out = append(cur.Out, "")
		default:
			if strings.HasPrefix(ln, " ") || strings.HasPrefix(ln, "\t") {
				return nil, fmt.Errorf("line %d: unexpected indentation", i+1)
			}
			cur = nil
			p := flushProse()
			p.Prose = append(p.Prose, ln)
		}
	}
	return t, nil
}

// continuesOutput reports whether the blank line at lines[i] sits inside an output block: the next
// non-blank line is indented and is not a command.
func continuesOutput(lines []string, i int) bool {
	for j := i + 1; j < len(lines); j++ {
		if lines[j] == "" {
			continue
		}
		return strings.HasPrefix(lines[j], indent) && !strings.HasPrefix(lines[j], indent+cmdPrefix)
	}
	return false
}

// Format writes a transcript in canonical form: Parse(Format(t)) equals t for any t that Parse
// can produce.
func Format(t *Transcript) string {
	var b strings.Builder
	for _, bl := range t.Blocks {
		if !bl.IsCmd {
			for _, p := range bl.Prose {
				b.WriteString(p + "\n")
			}
			continue
		}
		b.WriteString(indent + cmdPrefix + bl.Cmd + "\n")
		for i, o := range bl.Out {
			switch {
			case o != "":
				b.WriteString(indent + o + "\n")
			case trailingBlank(bl.Out, i):
				b.WriteString(indent + "\n")
			default:
				b.WriteString("\n")
			}
		}
	}
	for _, f := range t.Files {
		b.WriteString("-- " + f.Path + " --\n")
		for _, l := range f.Lines {
			b.WriteString(l + "\n")
		}
	}
	return b.String()
}

// trailingBlank reports whether every output line from i on is blank, so a bare empty line
// would read as a prose separator.
func trailingBlank(out []string, i int) bool {
	for _, o := range out[i:] {
		if o != "" {
			return false
		}
	}
	return true
}

// Section returns the file section called name.
func (t *Transcript) Section(name string) (string, bool) {
	for _, f := range t.Files {
		if f.Path == name {
			if len(f.Lines) == 0 {
				return "", true
			}
			return strings.Join(f.Lines, "\n") + "\n", true
		}
	}
	return "", false
}
