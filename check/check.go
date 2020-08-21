package check

import (
	"bytes"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/errata-ai/vale/core"
	"github.com/errata-ai/vale/data"
	"github.com/errata-ai/vale/rule"
	"github.com/errata-ai/vale/spell"
	"github.com/jdkato/prose/summarize"
	"github.com/jdkato/prose/tag"
	"github.com/jdkato/prose/transform"
	"github.com/jdkato/regexp"
	"gopkg.in/yaml.v2"
)

const (
	ignoreCase      = `(?i)`
	wordTemplate    = `(?m)\b(?:%s)\b`
	nonwordTemplate = `(?m)(?:%s)`
)

var defaultFilters = []*regexp.Regexp{
	regexp.MustCompile(`(?:\w+)?\.\w{1,4}\b`),
	regexp.MustCompile(`\b(?:[a-zA-Z]\.){2,}`),
	regexp.MustCompile(`0[xX][0-9a-fA-F]+`),
	regexp.MustCompile(`\w+-\w+`),
	regexp.MustCompile(`[A-Z]{1}[a-z]+[A-Z]+\w+`),
	regexp.MustCompile(`[0-9]`),
	regexp.MustCompile(`[A-Z]+$`),
	regexp.MustCompile(`\W`),
	regexp.MustCompile(`\w{3,}\.\w{3,}`),
	regexp.MustCompile(`@.*\b`),
}

type ruleFn func(string, *core.File) []core.Alert

// Manager controls the loading and validating of the check extension points.
type Manager struct {
	AllChecks map[string]Check
	Config    *core.Config
	Scopes    map[string]struct{}
}

// NewManager creates a new Manager and loads the rule definitions (that is,
// extended checks) specified by configuration.
func NewManager(config *core.Config) *Manager {
	var path string

	mgr := Manager{
		AllChecks: make(map[string]Check),
		Config:    config,
		Scopes:    make(map[string]struct{}),
	}

	// loadedStyles keeps track of the styles we've loaded as we go.
	loadedStyles := []string{}
	if mgr.Config.StylesPath == "" {
		// If we're not given a StylesPath, there's nothing left to look for.
		mgr.loadDefaultRules(loadedStyles, true)
		return &mgr
	}

	loadedStyles = append(loadedStyles, mgr.loadStyles(mgr.Config.GBaseStyles, loadedStyles)...)
	for _, styles := range mgr.Config.SBaseStyles {
		loadedStyles = append(loadedStyles, mgr.loadStyles(styles, loadedStyles)...)
	}

	for _, chk := range mgr.Config.Checks {
		// Load any remaining individual rules.
		if !strings.Contains(chk, ".") {
			// A rule must be associated with a style (i.e., "Style[.]Rule").
			continue
		}
		parts := strings.Split(chk, ".")
		if !core.StringInSlice(parts[0], loadedStyles) {
			// If this rule isn't part of an already-loaded style, we load it
			// individually.
			fName := parts[1] + ".yml"
			path = filepath.Join(mgr.Config.StylesPath, parts[0], fName)
			core.CheckError(mgr.loadCheck(fName, path), mgr.Config.Debug)
		}
	}

	// Finally, after reading the user's `StylesPath`, we load our built-in
	// styles:
	mgr.loadDefaultRules(loadedStyles, true)

	return &mgr
}

func makeRegexp(
	template string,
	noCase bool,
	word func() bool,
	callback func() string,
	append bool,
) string {
	regex := ""

	if word() {
		if template != "" {
			regex += template
		} else {
			regex += wordTemplate
		}
	} else {
		regex += nonwordTemplate
	}

	if append {
		regex += callback()
	} else {
		regex = callback() + regex
	}

	if noCase {
		regex = ignoreCase + regex
	}

	return regex
}

func formatMessages(msg string, desc string, subs ...string) (string, string) {
	return core.FormatMessage(msg, subs...), core.FormatMessage(desc, subs...)
}

func makeAlert(chk Definition, loc []int, txt string) core.Alert {
	match := txt[loc[0]:loc[1]]
	a := core.Alert{
		Check: chk.Name, Severity: chk.Level, Span: loc, Link: chk.Link,
		Match: match, Action: chk.Action}
	a.Message, a.Description = formatMessages(chk.Message, chk.Description, match)
	return a
}

func tokensMatch(token NLPToken, word tag.Token) bool {
	failedTag, err := regexp.MatchString(token.Tag, word.Tag)
	if core.CheckError(err, false) {
		failedTag = !failedTag

		failedTok := (token.re != nil && token.re.MatchString(word.Text) == token.Negate)

		if (token.Pattern == "" && failedTag) ||
			(token.Tag == "" && failedTok) ||
			(token.Tag != "" && token.Pattern != "") && (failedTag || failedTok) {
			return false
		}

		return true
	}
	return false
}

func sequenceMatches(idx int, chk Sequence, target, src string) ([]string, int) {
	toks := chk.Tokens
	text := []string{}

	sizeT := len(toks)
	words := core.TextToTokens(src, chk.needsTagging)
	index := 0

	for jdx, tok := range words {
		if tok.Text == target && !core.IntInSlice(jdx, chk.history) {
			index = jdx
			// We've found our context.
			if idx > 0 {
				// Check the left-end of the sequence:
				for i := 1; idx-i >= 0; i++ {
					word := words[jdx-i]
					text = append([]string{word.Text}, text...)

					mat := tokensMatch(toks[idx-i], word)
					opt := toks[idx-i].optional
					if !mat && !opt {
						return []string{}, index
					} else if mat && opt {
						break
					}
				}
			}
			if idx < sizeT {
				// Check the right-end of the sequence
				for i := 1; idx+i < sizeT; i++ {
					if i == 1 {
						text = append(text, words[index].Text)
					}
					word := words[jdx+i]
					text = append(text, word.Text)

					mat := tokensMatch(toks[idx+i], word)
					opt := toks[idx+i].optional
					if !mat && !opt {
						return []string{}, index
					} else if mat && opt {
						break
					}
				}
			}
			break
		}
	}

	return text, index
}

func stepsToString(steps []string) string {
	s := ""
	for _, step := range steps {
		if strings.HasPrefix(step, "'") {
			s += step
		} else {
			s += " " + step
		}
	}
	return strings.Trim(s, " ")
}

func checkConditional(txt string, chk Conditional, f *core.File, r []*regexp.Regexp) []core.Alert {
	alerts := []core.Alert{}

	// We first look for the consequent of the conditional statement.
	// For example, if we're ensuring that abbreviations have been defined
	// parenthetically, we'd have something like:
	//
	//     "WHO" [antecedent], "World Health Organization (WHO)" [consequent]
	//
	// In other words: if "WHO" exists, it must also have a definition -- which
	// we're currently looking for.
	matches := r[0].FindAllStringSubmatch(txt, -1)
	for _, mat := range matches {
		if len(mat) > 1 {
			// If we find one, we store it in a slice associated with this
			// particular file.
			f.Sequences = append(f.Sequences, mat[1])
		}
	}

	// Now we look for the antecedent.
	locs := r[1].FindAllStringIndex(txt, -1)
	for _, loc := range locs {
		s := txt[loc[0]:loc[1]]
		if !core.StringInSlice(s, f.Sequences) && !isMatch(chk.exceptRe, s) {
			// If we've found one (e.g., "WHO") and we haven't marked it as
			// being defined previously, send an Alert.
			alerts = append(alerts, makeAlert(chk.Definition, loc, txt))
		}
	}

	return alerts
}

func checkExistence(txt string, chk Existence, f *core.File, r *regexp.Regexp) []core.Alert {
	alerts := []core.Alert{}
	locs := r.FindAllStringIndex(txt, -1)
	for _, loc := range locs {
		alerts = append(alerts, makeAlert(chk.Definition, loc, txt))
	}
	return alerts
}

func checkOccurrence(txt string, chk Occurrence, f *core.File, r *regexp.Regexp) []core.Alert {
	alerts := []core.Alert{}

	locs := r.FindAllStringIndex(txt, -1)
	occurrences := len(locs)
	if occurrences > chk.Max || occurrences < chk.Min {
		// NOTE: We take only the first match (`locs[0]`) instead of the whole
		// scope (`txt`) to avoid having to fall back to string matching.
		//
		// See (core/util.go#initialPosition).
		a := makeAlert(chk.Definition, locs[0], txt)
		a.Message = chk.Message
		a.Description = chk.Description
		alerts = append(alerts, a)
	}

	return alerts
}

func checkRepetition(txt string, chk Repetition, f *core.File, r *regexp.Regexp) []core.Alert {
	var curr, prev string
	var hit bool
	var ploc []int
	var count int

	alerts := []core.Alert{}
	for _, loc := range r.FindAllStringIndex(txt, -1) {
		curr = strings.TrimSpace(txt[loc[0]:loc[1]])
		if chk.Ignorecase {
			hit = strings.ToLower(curr) == strings.ToLower(prev) && curr != ""
		} else {
			hit = curr == prev && curr != ""
		}

		hit = hit && (!chk.Alpha || core.IsLetter(curr))
		if hit {
			count++
		}

		if hit && count > chk.Max {
			if !strings.Contains(txt[ploc[0]:loc[1]], "\n") {
				floc := []int{ploc[0], loc[1]}
				a := makeAlert(chk.Definition, floc, txt)
				a.Message, a.Description = formatMessages(chk.Message,
					chk.Description, curr)
				alerts = append(alerts, a)
				count = 0
			}
		}
		ploc = loc
		prev = curr
	}
	return alerts
}

func checkSubstitution(txt string, chk Substitution, f *core.File, r *regexp.Regexp, repl []string) []core.Alert {
	alerts := []core.Alert{}
	pos := false

	// Leave early if we can to avoid calling `FindAllStringSubmatchIndex`
	// unnecessarily.
	if !r.MatchString(txt) {
		return alerts
	}

	for _, submat := range r.FindAllStringSubmatchIndex(txt, -1) {
		for idx, mat := range submat {
			if mat != -1 && idx > 0 && idx%2 == 0 {
				loc := []int{mat, submat[idx+1]}
				// Based on the current capture group (`idx`), we can determine
				// the associated replacement string by using the `repl` slice:
				expected := repl[(idx/2)-1]
				observed := strings.TrimSpace(txt[loc[0]:loc[1]])
				if expected != observed {
					if chk.POS != "" {
						// If we're given a POS pattern, check that it matches.
						//
						// If it doesn't match, the alert doesn't get added to
						// a File (i.e., `hide` == true).
						pos = core.CheckPOS(loc, chk.POS, txt)
					}
					if chk.Action.Name == "replace" && len(chk.Action.Params) == 0 {
						chk.Action.Params = strings.Split(expected, "|")
						expected = core.ToSentence(chk.Action.Params, "or")
					}
					a := core.Alert{
						Check: chk.Name, Severity: chk.Level, Span: loc,
						Link: chk.Link, Hide: pos, Match: observed,
						Action: chk.Action}

					a.Message, a.Description = formatMessages(chk.Message,
						chk.Description, expected, observed)

					alerts = append(alerts, a)
				}
			}
		}
	}

	return alerts
}

func checkConsistency(txt string, chk Consistency, f *core.File, r *regexp.Regexp, opts []string) []core.Alert {
	alerts := []core.Alert{}
	loc := []int{}

	matches := r.FindAllStringSubmatchIndex(txt, -1)
	for _, submat := range matches {
		for idx, mat := range submat {
			if mat != -1 && idx > 0 && idx%2 == 0 {
				loc = []int{mat, submat[idx+1]}
				f.Sequences = append(f.Sequences, r.SubexpNames()[idx/2])
			}
		}
	}

	if matches != nil && core.AllStringsInSlice(opts, f.Sequences) {
		chk.Name = chk.Extends
		alerts = append(alerts, makeAlert(chk.Definition, loc, txt))
	}
	return alerts
}

func checkCapitalization(txt string, chk Capitalization, f *core.File) []core.Alert {
	alerts := []core.Alert{}
	if !chk.Check(txt, chk.Exceptions, chk.exceptRe) {
		alerts = append(alerts, makeAlert(chk.Definition, []int{0, len(txt)}, txt))
	}
	return alerts
}

func checkSequence(txt string, chk Sequence, f *core.File) []core.Alert {
	var alerts []core.Alert

	for idx, tok := range chk.Tokens {
		if !tok.Negate && tok.Pattern != "" {
			for _, loc := range tok.re.FindAllStringIndex(txt, -1) {
				target := txt[loc[0]:loc[1]]
				// These are all possible violations in `txt`:
				steps, index := sequenceMatches(idx, chk, target, txt)
				chk.history = append(chk.history, index)

				if len(steps) > 0 {
					seq := stepsToString(steps)
					idx := strings.Index(txt, seq)

					a := core.Alert{
						Check: chk.Name, Severity: chk.Level, Link: chk.Link,
						Span: []int{idx, idx + len(seq)}, Hide: false,
						Match: seq, Action: chk.Action}

					a.Message, a.Description = formatMessages(chk.Message,
						chk.Description, steps...)

					alerts = append(alerts, a)
				}
			}
			break
		}
	}

	return alerts
}

func checkReadability(txt string, chk Readability, f *core.File) []core.Alert {
	var grade float64
	alerts := []core.Alert{}

	doc := summarize.NewDocument(txt)
	if core.StringInSlice("SMOG", chk.Metrics) {
		grade += doc.SMOG()
	}
	if core.StringInSlice("Gunning Fog", chk.Metrics) {
		grade += doc.GunningFog()
	}
	if core.StringInSlice("Coleman-Liau", chk.Metrics) {
		grade += doc.ColemanLiau()
	}
	if core.StringInSlice("Flesch-Kincaid", chk.Metrics) {
		grade += doc.FleschKincaid()
	}
	if core.StringInSlice("Automated Readability", chk.Metrics) {
		grade += doc.AutomatedReadability()
	}

	grade = grade / float64(len(chk.Metrics))
	if grade > chk.Grade {
		a := core.Alert{Check: chk.Name, Severity: chk.Level,
			Span: []int{0, len(txt)}, Link: chk.Link}
		a.Message, a.Description = formatMessages(chk.Message, chk.Description,
			fmt.Sprintf("%.2f", grade))
		alerts = append(alerts, a)
	}

	return alerts
}

func checkSpelling(txt string, chk Spelling, gs *spell.GoSpell, f *core.File) []core.Alert {
	alerts := []core.Alert{}

	// This ensures that we respect `.aff` entries like `ICONV ’ '`,
	// allowing us to avoid false positives.
	//
	// See https://github.com/errata-ai/vale/issues/148.
	txt = gs.InputConversion([]byte(txt))

OUTER:
	for _, word := range core.WordTokenizer.Tokenize(txt) {
		for _, filter := range chk.Filters {
			if filter.MatchString(word) {
				continue OUTER
			}
		}

		known := gs.Spell(word) || gs.Spell(strings.ToLower(word))
		if !known && !isMatch(chk.exceptRe, word) {
			offset := strings.Index(txt, word)
			loc := []int{offset, offset + len(word)}

			a := core.Alert{Check: chk.Name, Severity: chk.Level, Span: loc,
				Link: chk.Link, Match: word, Action: chk.Action}

			a.Message, a.Description = formatMessages(chk.Message,
				chk.Description, word)

			alerts = append(alerts, a)
		}
	}

	return alerts
}

func (mgr *Manager) addReadabilityCheck(chkName string, chkDef Readability) {
	if core.AllStringsInSlice(chkDef.Metrics, readabilityMetrics) {
		fn := func(text string, file *core.File) []core.Alert {
			return checkReadability(text, chkDef, file)
		}
		// NOTE: This is the only extension point that doesn't support scoping.
		// The reason for this is that we need to split on sentences to
		// calculate readability, which means that specifying a scope smaller
		// than a paragraph or including non-block level content (i.e.,
		// headings, list items or table cells) doesn't make sense.
		chkDef.Definition.Scope = "summary"
		mgr.updateAllChecks(chkDef.Definition, fn, "")
	}
}

func (mgr *Manager) addCapitalizationCheck(chkName string, chkDef Capitalization) {
	if chkDef.Match == "$title" {
		var tc *transform.TitleConverter
		if chkDef.Style == "Chicago" {
			tc = transform.NewTitleConverter(transform.ChicagoStyle)
		} else {
			tc = transform.NewTitleConverter(transform.APStyle)
		}
		chkDef.Check = func(s string, ignore []string, re *regexp.Regexp) bool {
			return title(s, ignore, re, tc)
		}
	} else if chkDef.Match == "$sentence" {
		chkDef.Check = func(s string, ignore []string, re *regexp.Regexp) bool {
			return sentence(s, ignore, chkDef.Indicators, re)
		}
	} else if f, ok := varToFunc[chkDef.Match]; ok {
		chkDef.Check = f
	} else {
		re, err := regexp.Compile(chkDef.Match)
		if !core.CheckError(err, mgr.Config.Debug) {
			return
		}
		chkDef.Check = func(s string, ignore []string, r *regexp.Regexp) bool {
			return re.MatchString(s) || core.StringInSlice(s, ignore)
		}
	}
	fn := func(text string, file *core.File) []core.Alert {
		return checkCapitalization(text, chkDef, file)
	}
	mgr.updateAllChecks(chkDef.Definition, fn, "")
}

func (mgr *Manager) addConsistencyCheck(chkName string, chkDef Consistency) {
	var chkRE string

	regex := makeRegexp(
		mgr.Config.WordTemplate,
		chkDef.Ignorecase,
		func() bool { return !chkDef.Nonword },
		func() string { return "" }, true)

	chkKey := strings.Split(chkName, ".")[1]
	count := 0
	for v1, v2 := range chkDef.Either {
		count += 2
		subs := []string{
			fmt.Sprintf("%s%d", chkKey, count), fmt.Sprintf("%s%d", chkKey, count+1)}

		chkRE = fmt.Sprintf("(?P<%s>%s)|(?P<%s>%s)", subs[0], v1, subs[1], v2)
		chkRE = fmt.Sprintf(regex, chkRE)
		re, err := regexp.Compile(chkRE)
		if core.CheckError(err, mgr.Config.Debug) {
			chkDef.Extends = chkName
			chkDef.Name = fmt.Sprintf("%s.%s", chkName, v1)
			fn := func(text string, file *core.File) []core.Alert {
				return checkConsistency(text, chkDef, file, re, subs)
			}
			mgr.updateAllChecks(chkDef.Definition, fn, re.String())
		}
	}
}

func (mgr *Manager) addExistenceCheck(chkName string, chkDef Existence) {

	regex := makeRegexp(
		mgr.Config.WordTemplate,
		chkDef.Ignorecase,
		func() bool { return !chkDef.Nonword && len(chkDef.Tokens) > 0 },
		func() string { return strings.Join(chkDef.Raw, "") },
		chkDef.Append)

	regex = fmt.Sprintf(regex, strings.Join(chkDef.Tokens, "|"))
	re, err := regexp.Compile(regex)
	if core.CheckError(err, mgr.Config.Debug) {
		fn := func(text string, file *core.File) []core.Alert {
			return checkExistence(text, chkDef, file, re)
		}
		mgr.updateAllChecks(chkDef.Definition, fn, re.String())
	}
}

func (mgr *Manager) addRepetitionCheck(chkName string, chkDef Repetition) {
	regex := ""
	if chkDef.Ignorecase {
		regex += ignoreCase
	}
	regex += `(` + strings.Join(chkDef.Tokens, "|") + `)`
	re, err := regexp.Compile(regex)
	if core.CheckError(err, mgr.Config.Debug) {
		fn := func(text string, file *core.File) []core.Alert {
			return checkRepetition(text, chkDef, file, re)
		}
		mgr.updateAllChecks(chkDef.Definition, fn, re.String())
	}
}

func (mgr *Manager) addOccurrenceCheck(chkName string, chkDef Occurrence) {
	regex := ""
	if chkDef.Ignorecase {
		regex += ignoreCase
	}
	regex += `(?:` + chkDef.Token + `)`
	re, err := regexp.Compile(regex)
	if core.CheckError(err, mgr.Config.Debug) {
		fn := func(text string, file *core.File) []core.Alert {
			return checkOccurrence(text, chkDef, file, re)
		}
		mgr.updateAllChecks(chkDef.Definition, fn, re.String())
	}
}

func (mgr *Manager) addConditionalCheck(chkName string, chkDef Conditional) {
	var re *regexp.Regexp
	var expression []*regexp.Regexp
	var err error

	re, err = regexp.Compile(chkDef.Second)
	if !core.CheckError(err, mgr.Config.Debug) {
		return
	}
	expression = append(expression, re)

	re, err = regexp.Compile(chkDef.First)
	if !core.CheckError(err, mgr.Config.Debug) {
		return
	}
	expression = append(expression, re)

	fn := func(text string, file *core.File) []core.Alert {
		return checkConditional(text, chkDef, file, expression)
	}
	// TODO: How do we support multiple patterns?
	mgr.updateAllChecks(chkDef.Definition, fn, "")
}

func (mgr *Manager) addSubstitutionCheck(chkName string, chkDef Substitution) {
	tokens := ""

	regex := makeRegexp(
		mgr.Config.WordTemplate,
		chkDef.Ignorecase,
		func() bool { return !chkDef.Nonword },
		func() string { return "" }, true)

	replacements := []string{}
	for regexstr, replacement := range chkDef.Swap {
		opens := strings.Count(regexstr, "(")
		if opens != strings.Count(regexstr, "?:") &&
			opens != strings.Count(regexstr, `\(`) {
			// We rely on manually-added capture groups to associate a match
			// with its replacement -- e.g.,
			//
			//    `(foo)|(bar)`, [replacement1, replacement2]
			//
			// where the first capture group ("foo") corresponds to the first
			// element of the replacements slice ("replacement1"). This means
			// that we can only accept non-capture groups from the user (the
			// indexing would be mixed up otherwise).
			//
			// TODO: Should we change this? Perhaps by creating a map of regex
			// to replacements?
			continue
		}
		tokens += `(` + regexstr + `)|`
		replacements = append(replacements, replacement)
	}

	regex = fmt.Sprintf(regex, strings.TrimRight(tokens, "|"))
	re, err := regexp.Compile(regex)
	if core.CheckError(err, mgr.Config.Debug) {
		fn := func(text string, file *core.File) []core.Alert {
			return checkSubstitution(text, chkDef, file, re, replacements)
		}
		mgr.updateAllChecks(chkDef.Definition, fn, re.String())
	}
}

func (mgr *Manager) addSpellingCheck(chkName string, chkDef Spelling) {
	var model *spell.GoSpell
	var err error

	affloc := core.FindAsset(mgr.Config, chkDef.Aff)
	dicloc := core.FindAsset(mgr.Config, chkDef.Dic)
	if core.FileExists(affloc) && core.FileExists(dicloc) {
		model, err = spell.NewGoSpell(affloc, dicloc)
	} else {
		// Fall back to the defaults:
		aff, _ := data.Asset("data/en_US-web.aff")
		dic, _ := data.Asset("data/en_US-web.dic")
		model, err = spell.NewGoSpellReader(
			bytes.NewReader(aff), bytes.NewReader(dic))
	}

	for _, ignore := range chkDef.Ignore {
		vocab := filepath.Join(mgr.Config.StylesPath, ignore)
		if chkName == "Vale.Spelling" && mgr.Config.Project != "" {
			// Special case: Project support
			vocab = filepath.Join(
				mgr.Config.StylesPath,
				"Vocab",
				mgr.Config.Project,
				ignore)
		}
		_, exists := model.AddWordListFile(vocab)
		if exists != nil {
			vocab, _ = filepath.Abs(ignore)
			_, exists = model.AddWordListFile(vocab)
			core.CheckError(exists, mgr.Config.Debug)
		}
	}

	if !chkDef.Custom {
		chkDef.Filters = append(chkDef.Filters, defaultFilters...)
	}

	fn := func(text string, file *core.File) []core.Alert {
		return checkSpelling(text, chkDef, model, file)
	}

	if core.CheckError(err, mgr.Config.Debug) {
		mgr.updateAllChecks(chkDef.Definition, fn, "")
	}
}

func (mgr *Manager) addSequenceCheck(chkName string, chkDef Sequence) {
	for i, token := range chkDef.Tokens {
		if !chkDef.needsTagging && token.Tag != "" {
			chkDef.needsTagging = true
		}

		if token.Pattern != "" {
			regex := makeRegexp(
				mgr.Config.WordTemplate,
				chkDef.Ignorecase,
				func() bool { return true },
				func() string { return "" },
				false)
			regex = fmt.Sprintf(regex, token.Pattern)

			re, err := regexp.Compile(regex)
			if core.CheckError(err, mgr.Config.Debug) {
				chkDef.Tokens[i].re = re
			}
		}

	}
	fn := func(text string, file *core.File) []core.Alert {
		return checkSequence(text, chkDef, file)
	}
	chkDef.Definition.Scope = "summary"
	mgr.updateAllChecks(chkDef.Definition, fn, "")
}

func (mgr *Manager) updateAllChecks(chkDef Definition, fn ruleFn, pattern string) {
	chk := Check{
		Rule:    fn,
		Extends: chkDef.Extends,
		Code:    chkDef.Code,
		Pattern: pattern,
		Limit:   chkDef.Limit,
	}
	chk.Level = core.LevelToInt[chkDef.Level]
	chk.Scope = core.Selector{Value: chkDef.Scope}
	mgr.AllChecks[chkDef.Name] = chk
}

func validateDefinition(generic map[string]interface{}, name string) error {
	msg := name + ": %s!"
	if point, ok := generic["extends"]; !ok {
		return fmt.Errorf(msg, "missing extension point")
	} else if !core.StringInSlice(point.(string), extensionPoints) {
		return fmt.Errorf(msg, "unknown extension point")
	} else if _, ok := generic["message"]; !ok {
		return fmt.Errorf(msg, "missing message")
	}
	return nil
}

func (mgr *Manager) addCheck(file []byte, chkName string) error {
	// Load the rule definition.
	generic := map[string]interface{}{}
	err := yaml.Unmarshal(file, &generic)
	if err != nil {
		return fmt.Errorf("%s: %s", chkName, err.Error())
	} else if defErr := validateDefinition(generic, chkName); defErr != nil {
		return defErr
	}

	// Set default values, if necessary.
	generic["name"] = chkName
	if level, ok := mgr.Config.RuleToLevel[chkName]; ok {
		generic["level"] = level
	} else if _, ok := generic["level"]; !ok {
		generic["level"] = "warning"
	}
	if _, ok := generic["scope"]; !ok {
		generic["scope"] = "text"
	}

	extends := generic["extends"].(string)
	if builder, hasBuilder := checkBuilders[extends]; hasBuilder {
		builder(chkName, generic, mgr)
	}

	base := strings.Split(generic["scope"].(string), ".")[0]
	mgr.Scopes[base] = struct{}{}

	return nil
}

func (mgr *Manager) loadExternalStyle(path string) {
	err := mgr.Config.FsWrapper.Walk(path,
		func(fp string, fi os.FileInfo, err error) error {
			if err != nil || fi.IsDir() {
				return err
			}
			core.CheckError(mgr.loadCheck(fi.Name(), fp), mgr.Config.Debug)
			return nil
		})
	core.CheckError(err, mgr.Config.Debug)
}

func (mgr *Manager) loadCheck(fName string, fp string) error {
	if strings.HasSuffix(fName, ".yml") {
		f, err := mgr.Config.FsWrapper.ReadFile(fp)
		if !core.CheckError(err, mgr.Config.Debug) {
			return err
		}

		style := filepath.Base(filepath.Dir(fp))
		chkName := style + "." + strings.Split(fName, ".")[0]
		if _, ok := mgr.AllChecks[chkName]; !ok {
			return mgr.addCheck(f, chkName)
		}
	}
	return nil
}

func (mgr *Manager) loadDefaultRules(loaded []string, load bool) {
	if load {
		for _, style := range defaultStyles {
			if core.StringInSlice(style, loaded) {
				// The user has a style on their `StylesPath` with the same name as
				// a built-in style.
				continue
			}
			rules, _ := rule.AssetDir(filepath.Join("rule", style))
			for _, name := range rules {
				b, err := rule.Asset(filepath.Join("rule", style, name))
				if err != nil {
					continue
				}
				identifier := strings.Join([]string{
					style, strings.Split(name, ".")[0]}, ".")
				core.CheckError(mgr.addCheck(b, identifier), mgr.Config.Debug)
			}
		}
	}
	mgr.loadVocabRules(mgr.Config)
}

func (mgr *Manager) loadStyles(styles []string, loaded []string) []string {
	var found []string

	baseDir := mgr.Config.StylesPath
	for _, style := range styles {
		p := filepath.Join(baseDir, style)
		if core.StringInSlice(style, loaded) || core.StringInSlice(style, defaultStyles) {
			// We've already loaded this style.
			continue
		} else if found, _ := mgr.Config.FsWrapper.DirExists(p); !found {
			core.CheckError(errors.New("missing style: '"+style+"'"), mgr.Config.Debug)
			continue
		}
		mgr.loadExternalStyle(p)
		found = append(found, style)
	}

	return found
}

func (mgr *Manager) loadVocabRules(config *core.Config) {
	if len(config.AcceptedTokens) > 0 {
		vocab := Substitution{}
		vocab.Extends = "substitution"
		vocab.Definition.Name = "Vale.Terms"
		vocab.Definition.Level = "error"
		vocab.Definition.Message = "Use '%s' instead of '%s'."
		vocab.Scope = "text"
		vocab.Ignorecase = true
		vocab.Swap = make(map[string]string)
		for term := range config.AcceptedTokens {
			if core.IsPhrase(term) {
				vocab.Swap[strings.ToLower(term)] = term
			}
		}
		mgr.addSubstitutionCheck("Vale.Terms", vocab)
	}

	if len(config.RejectedTokens) > 0 {
		avoid := Existence{}
		avoid.Extends = "existence"
		avoid.Definition.Name = "Vale.Avoid"
		avoid.Definition.Level = "error"
		avoid.Definition.Message = "Avoid using '%s'."
		avoid.Scope = "text"
		avoid.Ignorecase = false
		for term := range config.RejectedTokens {
			avoid.Tokens = append(avoid.Tokens, term)
		}
		mgr.addExistenceCheck("Vale.Avoid", avoid)
	}

	if config.LTPath != "" {
		mgr.updateAllChecks(Definition{
			Extends: "existence",
			Level:   "warning",
			Name:    "LanguageTool.Grammar",
			Scope:   "summary",
		}, func(text string, file *core.File) []core.Alert {
			return rule.CheckWithLT(
				text, config.LTPath, file, config.Debug)
		}, "")
	}
}

// Compile returns a compiled check.
//
// TODO: Should we just expose `loadCheck`?
func (mgr *Manager) Compile(name, path string) error {
	return mgr.loadCheck(name, path)
}

// AddCheck adds the given check.
//
// TODO: Should we just expose `addCheck`?
func (mgr *Manager) AddCheck(content []byte, chkName string) error {
	return mgr.addCheck(content, chkName)
}
