package check

import (
	"fmt"
	"reflect"
	"strings"

	"github.com/errata-ai/vale/core"
	"github.com/jdkato/regexp"
	"github.com/mitchellh/mapstructure"
	"gopkg.in/validator.v2"
)

var defaultStyles = []string{"Vale"}
var extensionPoints = []string{
	"capitalization",
	"conditional",
	"consistency",
	"existence",
	"occurrence",
	"repetition",
	"substitution",
	"readability",
	"spelling",
	"sequence",
}

// A Check implements a single rule.
type Check struct {
	Code    bool
	Extends string
	Level   int
	Limit   int
	Pattern string
	Rule    ruleFn
	Scope   core.Selector
}

// A RuleError represents an error encoutered while processing an external YAML
// file.
//
// The idea here is that we can't panic due to the nature of how Vale is used:
//
type RuleError struct {
}

// Definition holds the common attributes of rule definitions.
type Definition struct {
	Action      core.Action
	Code        bool
	Description string
	Extends     string `validate:"regexp=^[a-zA-Z]*$"`
	Level       string
	Limit       int
	Link        string
	Message     string
	Name        string
	Scope       string
}

// NLPToken represents a token of text with NLP-related attributes.
type NLPToken struct {
	Pattern string
	Negate  bool
	Tag     string
	Skip    int

	re       *regexp.Regexp
	optional bool
}

// Existence checks for the present of Tokens.
type Existence struct {
	Definition `mapstructure:",squash"`
	// `append` (`bool`): Adds `raw` to the end of `tokens`, assuming both are defined.
	Append bool
	// `ignorecase` (`bool`): Makes all matches case-insensitive.
	Ignorecase bool
	// `nonword` (`bool`): Removes the default word boundaries (`\b`).
	Nonword bool
	// `raw` (`array`): A list of tokens to be concatenated into a pattern.
	Raw []string
	// `tokens` (`array`): A list of tokens to be transformed into a non-capturing group.
	Tokens []string
}

// Substitution switches the values of Swap for its keys.
type Substitution struct {
	Definition `mapstructure:",squash"`
	// `ignorecase` (`bool`): Makes all matches case-insensitive.
	Ignorecase bool
	// `nonword` (`bool`): Removes the default word boundaries (`\b`).
	Nonword bool
	// `swap` (`map`): A sequence of `observed: expected` pairs.
	Swap map[string]string
	// `pos` (`string`): A regular expression matching tokens to parts of speech.
	POS string
}

// Occurrence counts the number of times Token appears.
type Occurrence struct {
	Definition `mapstructure:",squash"`
	// `ignorecase` (`bool`): Makes all matches case-insensitive.
	Ignorecase bool
	// `max` (`int`): The maximum amount of times `token` may appear in a given scope.
	Max int
	// `min` (`int`): The minimum amount of times `token` has to appear in a given scope.
	Min int
	// `token` (`string`): The token of interest.
	Token string
}

// Repetition looks for repeated uses of Tokens.
type Repetition struct {
	Definition `mapstructure:",squash"`
	Max        int
	// `ignorecase` (`bool`): Makes all matches case-insensitive.
	Ignorecase bool
	// `alpha` (`bool`): Limits all matches to alphanumeric tokens.
	Alpha bool
	// `tokens` (`array`): A list of tokens to be transformed into a non-capturing group.
	Tokens []string
}

// Consistency ensures that the keys and values of Either don't both exist.
type Consistency struct {
	Definition `mapstructure:",squash"`
	// `nonword` (`bool`): Removes the default word boundaries (`\b`).
	Nonword bool
	// `ignorecase` (`bool`): Makes all matches case-insensitive.
	Ignorecase bool
	// `either` (`map`): A map of `option 1: option 2` pairs, of which only one may appear.
	Either map[string]string
}

// Conditional ensures that the present of First ensures the present of Second.
type Conditional struct {
	Definition `mapstructure:",squash"`
	// `ignorecase` (`bool`): Makes all matches case-insensitive.
	Ignorecase bool
	// `first` (`string`): The antecedent of the statement.
	First string
	// `second` (`string`): The consequent of the statement.
	Second string
	// `exceptions` (`array`): An array of strings to be ignored.
	Exceptions []string

	exceptRe *regexp.Regexp
}

// Capitalization checks the case of a string.
type Capitalization struct {
	Definition `mapstructure:",squash"`
	// `match` (`string`): $title, $sentence, $lower, $upper, or a pattern.
	Match string
	Check func(s string, ignore []string, re *regexp.Regexp) bool
	// `style` (`string`): AP or Chicago; only applies when match is set to $title.
	Style string
	// `exceptions` (`array`): An array of strings to be ignored.
	Exceptions []string
	// `indicators` (`array`): An array of suffixes that indicate the next
	// token should be ignored.
	Indicators []string

	exceptRe *regexp.Regexp
}

// Readability checks the reading grade level of text.
type Readability struct {
	Definition `mapstructure:",squash"`
	// `metrics` (`array`): One or more of Gunning Fog, Coleman-Liau, Flesch-Kincaid, SMOG, and Automated Readability.
	Metrics []string
	// `grade` (`float`): The highest acceptable score.
	Grade float64
}

// Spelling checks text against a Hunspell dictionary.
type Spelling struct {
	Definition `mapstructure:",squash"`
	// `aff` (`string`): The fully-qualified path to a Hunspell-compatible `.aff` file.
	Aff string
	// `custom` (`bool`): Turn off the default filters for acronyms, abbreviations, and numbers.
	Custom bool
	// `dic` (`string`): The fully-qualified path to a Hunspell-compatible `.dic` file.
	Dic string
	// `filters` (`array`): An array of patterns to ignore during spell checking.
	Filters []*regexp.Regexp
	// `ignore` (`array`): An array of relative paths (from `StylesPath`) to files consisting of one word per line to ignore.
	Ignore     []string
	Exceptions []string
	Threshold  int

	exceptRe *regexp.Regexp
}

// Sequence looks for a user-defined sequence of tokens.
type Sequence struct {
	Definition `mapstructure:",squash"`
	Ignorecase bool
	Tokens     []NLPToken

	needsTagging bool
	history      []int
}

type baseCheck map[string]interface{}

var checkBuilders = map[string]func(name string, generic baseCheck, mgr *Manager) error{
	"existence": func(name string, generic baseCheck, mgr *Manager) error {
		def := Existence{}
		if err := mapstructure.Decode(generic, &def); err == nil {
			mgr.addExistenceCheck(name, def)
		}
		return nil
	},
	"substitution": func(name string, generic baseCheck, mgr *Manager) error {
		def := Substitution{}

		err := mapstructure.Decode(generic, &def)
		if err != nil {
			fmt.Println("BYE", err)
			return err
		}

		err = validator.Validate(def)
		if err != nil {
			return err
		}

		mgr.addSubstitutionCheck(name, def)
		return nil
	},
	"occurrence": func(name string, generic baseCheck, mgr *Manager) error {
		def := Occurrence{}
		if err := mapstructure.Decode(generic, &def); err == nil {
			mgr.addOccurrenceCheck(name, def)
		}
		return nil
	},
	"repetition": func(name string, generic baseCheck, mgr *Manager) error {
		def := Repetition{}
		if err := mapstructure.Decode(generic, &def); err == nil {
			mgr.addRepetitionCheck(name, def)
		}
		return nil
	},
	"consistency": func(name string, generic baseCheck, mgr *Manager) error {
		def := Consistency{}
		if err := mapstructure.Decode(generic, &def); err == nil {
			mgr.addConsistencyCheck(name, def)
		}
		return nil
	},
	"conditional": func(name string, generic baseCheck, mgr *Manager) error {
		def := Conditional{}
		if err := mapstructure.Decode(generic, &def); err == nil {
			for term := range mgr.Config.AcceptedTokens {
				def.Exceptions = append(def.Exceptions, term)
			}
			def.exceptRe = regexp.MustCompile(strings.Join(def.Exceptions, "|"))
			mgr.addConditionalCheck(name, def)
		}
		return nil
	},
	"capitalization": func(name string, generic baseCheck, mgr *Manager) error {
		def := Capitalization{}
		if err := mapstructure.Decode(generic, &def); err == nil {
			for term := range mgr.Config.AcceptedTokens {
				def.Exceptions = append(def.Exceptions, term)
			}
			def.exceptRe = regexp.MustCompile(strings.Join(def.Exceptions, "|"))
			mgr.addCapitalizationCheck(name, def)
		}
		return nil
	},
	"readability": func(name string, generic baseCheck, mgr *Manager) error {
		def := Readability{}
		if err := mapstructure.Decode(generic, &def); err == nil {
			mgr.addReadabilityCheck(name, def)
		}
		return nil
	},
	"spelling": func(name string, generic baseCheck, mgr *Manager) error {
		def := Spelling{}

		if generic["filters"] != nil {
			// We pre-compile user-provided filters for efficiency.
			//
			// NOTE: This makes a big difference: ~50s -> ~13s.
			for _, filter := range generic["filters"].([]interface{}) {
				if pat, e := regexp.Compile(filter.(string)); e == nil {
					// TODO: Should we report malformed patterns?
					def.Filters = append(def.Filters, pat)
				}
			}
			delete(generic, "filters")
		}

		if generic["ignore"] != nil {
			// Backwards compatibility: we need to be able to accept a single
			// or an array.
			if reflect.TypeOf(generic["ignore"]).String() == "string" {
				def.Ignore = append(def.Ignore, generic["ignore"].(string))
			} else {
				for _, ignore := range generic["ignore"].([]interface{}) {
					def.Ignore = append(def.Ignore, ignore.(string))
				}
			}
			delete(generic, "ignore")
		}

		for term := range mgr.Config.AcceptedTokens {
			def.Exceptions = append(def.Exceptions, term)
			def.exceptRe = regexp.MustCompile(
				ignoreCase + strings.Join(def.Exceptions, "|"))
		}

		if err := mapstructure.Decode(generic, &def); err == nil {
			mgr.addSpellingCheck(name, def)
		}
		return nil
	},
	"sequence": func(name string, generic baseCheck, mgr *Manager) error {
		def := Sequence{}

		for _, token := range generic["tokens"].([]interface{}) {
			tok := NLPToken{}
			mapstructure.Decode(token, &tok)
			def.Tokens = append(def.Tokens, tok)

			tok.optional = true
			for i := tok.Skip; i > 0; i-- {
				def.Tokens = append(def.Tokens, tok)
			}
		}
		delete(generic, "tokens")

		if err := mapstructure.Decode(generic, &def); err == nil {
			mgr.addSequenceCheck(name, def)
		}
		return nil
	},
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
