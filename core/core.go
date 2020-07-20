package core

import (
	"bufio"
	"bytes"
	"io/ioutil"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"unicode/utf8"

	"github.com/gobwas/glob"
	"github.com/jdkato/prose/tag"
	"github.com/jdkato/prose/tokenize"
	"github.com/schollz/closestmatch"
)

// A File represents a linted text file.
type File struct {
	Alerts     []Alert           // all alerts associated with this file
	BaseStyles []string          // base style assigned in .vale
	Checks     map[string]bool   // syntax-specific checks assigned in .vale
	ChkToCtx   map[string]string // maps a temporary context to a particular check
	Comments   map[string]bool   // comment control statements
	Content    string            // the raw file contents
	Counts     map[string]int    // word counts
	Format     string            // 'code', 'markup' or 'prose'
	Lines      []string          // the File's Content split into lines
	Command    string            // a user-provided parsing CLI command
	NormedExt  string            // the normalized extension (see util/format.go)
	Path       string            // the full path
	Transform  string            // XLST transform
	RealExt    string            // actual file extension
	Scanner    *bufio.Scanner    // used by lintXXX functions
	Sequences  []string          // tracks various info (e.g., defined abbreviations)
	Simple     bool
	Summary    bytes.Buffer // holds content to be included in summarization checks

	history map[string]int
	matcher *closestmatch.ClosestMatch
}

// An Action represents a possible solution to an Alert.
//
// The possible
type Action struct {
	Name   string   // the name of the action -- e.g, 'replace'
	Params []string // a slice of parameters for the given action
}

// An Alert represents a potential error in prose.
type Alert struct {
	Action      Action // a possible solution
	Check       string // the name of the check
	Description string // why `Message` is meaningful
	Line        int    // the source line
	Link        string // reference material
	Message     string // the output message
	Severity    string // 'suggestion', 'warning', or 'error'
	Span        []int  // the [begin, end] location within a line
	Match       string // the actual matched text

	Hide bool `json:"-"` // should we hide this alert?
}

// A Plugin provides a means of extending Vale.
type Plugin struct {
	Scope string
	Level string
	Rule  func(string, *File) []Alert
}

// A Selector represents a named section of text.
type Selector struct {
	Value string // e.g., text.comment.line.py
}

// Sections splits a Selector into its parts -- e.g., text.comment.line.py ->
// []string{"text", "comment", "line", "py"}.
func (s Selector) Sections() []string { return strings.Split(s.Value, ".") }

// Contains determines if all if sel's sections are in s.
func (s Selector) Contains(sel Selector) bool {
	return AllStringsInSlice(sel.Sections(), s.Sections())
}

// Equal determines if sel == s.
func (s Selector) Equal(sel Selector) bool { return s.Value == sel.Value }

// Has determines if s has a part equal to scope.
func (s Selector) Has(scope string) bool {
	return StringInSlice(scope, s.Sections())
}

// ByPosition sorts Alerts by line and column.
type ByPosition []Alert

func (a ByPosition) Len() int      { return len(a) }
func (a ByPosition) Swap(i, j int) { a[i], a[j] = a[j], a[i] }
func (a ByPosition) Less(i, j int) bool {
	ai, aj := a[i], a[j]

	if ai.Line != aj.Line {
		return ai.Line < aj.Line
	}
	return ai.Span[0] < aj.Span[0]
}

// ByName sorts Files by their path.
type ByName []*File

func (a ByName) Len() int      { return len(a) }
func (a ByName) Swap(i, j int) { a[i], a[j] = a[j], a[i] }
func (a ByName) Less(i, j int) bool {
	ai, aj := a[i], a[j]
	return ai.Path < aj.Path
}

// NewFile initilizes a File.
func NewFile(src string, config *Config) *File {
	var scanner *bufio.Scanner
	var format, ext string
	var fbytes []byte

	if FileExists(src) {
		fbytes, _ = ioutil.ReadFile(src)
		scanner = bufio.NewScanner(bytes.NewReader(fbytes))
		if config.InExt != ".txt" {
			ext, format = FormatFromExt(config.InExt, config.Formats)
		} else {
			ext, format = FormatFromExt(src, config.Formats)
		}
	} else {
		scanner = bufio.NewScanner(strings.NewReader(src))
		ext, format = FormatFromExt(config.InExt, config.Formats)
		fbytes = []byte(src)
		src = "stdin" + config.InExt
	}

	baseStyles := config.GBaseStyles
	for sec, styles := range config.SBaseStyles {
		if pat, found := config.SecToPat[sec]; found && pat.Match(src) {
			baseStyles = styles
			break
		}
	}

	checks := make(map[string]bool)
	for sec, smap := range config.SChecks {
		if pat, found := config.SecToPat[sec]; found && pat.Match(src) {
			checks = smap
			break
		}
	}

	transform := ""
	for sec, p := range config.Stylesheets {
		pat, err := glob.Compile(sec)
		if CheckError(err, config.Debug) && pat.Match(src) {
			transform = p
			break
		}
	}

	scanner.Split(SplitLines)
	content := PrepText(string(fbytes))
	lines := strings.SplitAfter(content, "\n")
	file := File{
		Path: src, NormedExt: ext, Format: format, RealExt: filepath.Ext(src),
		BaseStyles: baseStyles, Checks: checks, Scanner: scanner, Lines: lines,
		Comments: make(map[string]bool), Content: content, history: make(map[string]int),
		Simple: config.Simple, Transform: transform,
		matcher: closestmatch.New(lines, []int{2}),
	}

	return &file
}

// SortedAlerts returns all of f's alerts sorted by line and column.
func (f *File) SortedAlerts() []Alert {
	sort.Sort(ByPosition(f.Alerts))
	return f.Alerts
}

// FindLoc calculates the line and span of an Alert.
func (f *File) FindLoc(ctx, s, m string, pad, count int, loc []int) (int, []int) {
	var length int
	var lines []string

	pos, substring := initialPosition(ctx, m, f.matcher)
	if pos < 0 {
		// Shouldn't happen ...
		return pos, []int{0, 0}
	}

	if f.Format == "markup" && !f.Simple {
		lines = f.Lines
	} else {
		lines = strings.SplitAfter(ctx, "\n")
	}

	counter := 0
	for idx, l := range lines {
		length = utf8.RuneCountInString(l)
		if (counter + length) >= pos {
			loc[0] = (pos - counter) + pad
			loc[1] = loc[0] + utf8.RuneCountInString(substring) - 1
			extent := length + pad
			if loc[1] > extent {
				loc[1] = extent
			}
			return count - (len(lines) - (idx + 1)), loc
		}
		counter += length
	}

	return count, loc
}

// FormatAlert ensures that all required fields have data.
func FormatAlert(a *Alert, level int, name string) {
	if a.Severity == "" {
		a.Severity = AlertLevels[level]
	}
	if a.Check == "" {
		a.Check = name
	}
}

// AddAlert calculates the in-text location of an Alert and adds it to a File.
func (f *File) AddAlert(a Alert, ctx, txt string, lines, pad int) {
	if old, ok := f.ChkToCtx[a.Check]; ok {
		ctx = old
	}

	a.Line, a.Span = f.FindLoc(ctx, txt, a.Match, pad, lines, a.Span)
	if a.Span[0] > 0 {
		f.ChkToCtx[a.Check], _ = Substitute(ctx, a.Match, '#')
		if !a.Hide {
			// Ensure that we're not double-reporting an Alert:
			entry := strings.Join([]string{
				strconv.Itoa(a.Line),
				strconv.Itoa(a.Span[0]),
				a.Check}, "-")
			if _, found := f.history[entry]; !found {
				f.Alerts = append(f.Alerts, a)
				f.history[entry] = 1
			}
		}
	}
}

var commentControlRE = regexp.MustCompile(`^vale (\w+.\w+) = (YES|NO)$`)

// UpdateComments sets a new status based on comment.
func (f *File) UpdateComments(comment string) {
	if comment == "vale off" {
		f.Comments["off"] = true
	} else if comment == "vale on" {
		f.Comments["off"] = false
	} else if commentControlRE.MatchString(comment) {
		check := commentControlRE.FindStringSubmatch(comment)
		if len(check) == 3 {
			f.Comments[check[1]] = check[2] == "NO"
		}
	}
}

// QueryComments checks if there has been an in-text comment for this check.
func (f *File) QueryComments(check string) bool {
	if !f.Comments["off"] {
		if status, ok := f.Comments[check]; ok {
			return status
		}
	}
	return f.Comments["off"]
}

// ResetComments resets the state of all checks back to active.
func (f *File) ResetComments() {
	for check := range f.Comments {
		if check != "off" {
			f.Comments[check] = false
		}
	}
}

// WordTokenizer splits text into words.
var WordTokenizer = tokenize.NewRegexpTokenizer(
	`[\p{L}[\p{N}]+(?:\.\w{2,4}\b)|(?:[A-Z]\.){2,}|[\p{L}[\p{N}]+['-][\p{L}-[\p{N}]+|[\p{L}[\p{N}@]+`, false, true)

// SentenceTokenizer splits text into sentences.
var SentenceTokenizer = tokenize.NewPunktSentenceTokenizer()

// Tagger tags a sentence.
//
// We wait to initilize it until we need it since it's slow (~1s) and we may
// not need it.
var Tagger *tag.PerceptronTagger
