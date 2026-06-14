package audit

import (
	"fmt"
	"regexp"
)

const masked = "[MASKED]"

// builtinPatterns are always active when masking is enabled.
// They cover the most common PII formats encountered in financial/enterprise systems.
var builtinPatterns = []string{
	`\b\d{16}\b`,                                                    // 16-digit payment card number
	`\b\d{12}\b`,                                                    // 12-digit IIN (Kazakhstan national ID)
	`\b[a-zA-Z0-9._%+\-]+@[a-zA-Z0-9.\-]+\.[a-zA-Z]{2,}\b`,        // email address
	`(?:\+7|8)[\s\-]?\(?\d{3}\)?[\s\-]?\d{3}[\s\-]?\d{2}[\s\-]?\d{2}\b`, // RU/KZ phone number
}

// Masker replaces sensitive patterns in strings with [MASKED].
type Masker struct {
	re *regexp.Regexp // single compiled alternation of all patterns
}

// NewMasker compiles builtinPatterns plus any extra patterns from config.
// Returns an error if any pattern fails to compile.
func NewMasker(extraPatterns []string) (*Masker, error) {
	all := make([]string, 0, len(builtinPatterns)+len(extraPatterns))
	all = append(all, builtinPatterns...)
	all = append(all, extraPatterns...)

	// Validate each pattern individually for a useful error message.
	for _, p := range extraPatterns {
		if _, err := regexp.Compile(p); err != nil {
			return nil, fmt.Errorf("invalid mask pattern %q: %w", p, err)
		}
	}

	// Combine into one regex for a single-pass replace.
	combined := "(?:" + joinOr(all) + ")"
	re, err := regexp.Compile(combined)
	if err != nil {
		return nil, fmt.Errorf("compile combined mask pattern: %w", err)
	}

	return &Masker{re: re}, nil
}

// Mask replaces all sensitive substrings in s with [MASKED].
func (m *Masker) Mask(s string) string {
	return m.re.ReplaceAllString(s, masked)
}

func joinOr(patterns []string) string {
	if len(patterns) == 0 {
		return ""
	}
	result := patterns[0]
	for _, p := range patterns[1:] {
		result += "|" + p
	}
	return result
}
