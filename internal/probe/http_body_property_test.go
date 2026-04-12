package probe

import (
	"fmt"
	"reflect"
	"regexp"
	"strings"
	"testing"

	"netsonar/internal/config"

	"github.com/leanovate/gopter"
	"github.com/leanovate/gopter/gen"
	"github.com/leanovate/gopter/prop"
)

// genSafeBody generates a random printable ASCII body string (1–200 chars).
func genSafeBody() gopter.Gen {
	return gen.IntRange(1, 200).FlatMap(func(v interface{}) gopter.Gen {
		n := v.(int)
		return gen.SliceOfN(n, gen.AlphaNumChar()).Map(func(chars []rune) string {
			return string(chars)
		})
	}, reflect.TypeFor[string]())
}

// genSubstring generates a short alphanumeric substring (1–20 chars) suitable
// for use as a body_match_string pattern.
func genSubstring() gopter.Gen {
	return gen.IntRange(1, 20).FlatMap(func(v interface{}) gopter.Gen {
		n := v.(int)
		return gen.SliceOfN(n, gen.AlphaNumChar()).Map(func(chars []rune) string {
			return string(chars)
		})
	}, reflect.TypeFor[string]())
}

// genSafeRegexLiteral generates a short alphanumeric string that is always a
// valid regex (no special characters). This avoids generating invalid regex
// patterns that would confuse the property assertions.
func genSafeRegexLiteral() gopter.Gen {
	return genSubstring()
}

// TestPropertyHTTPBodySubstringMatch verifies Property 14a:
// When body_match_string is configured, matchBody returns true if and only if
// the body contains the configured substring.
//
// **Validates: Requirement 12.2, 12.3, 12.4**
func TestPropertyHTTPBodySubstringMatch(t *testing.T) {
	parameters := gopter.DefaultTestParameters()
	parameters.MinSuccessfulTests = 300
	parameters.MaxSize = 50
	properties := gopter.NewProperties(parameters)

	// 14a-1: Substring present → BodyMatch=true.
	properties.Property("substring present in body implies BodyMatch=true", prop.ForAll(
		func(prefix string, sub string, suffix string) (bool, error) {
			body := prefix + sub + suffix
			opts := config.ProbeOptions{BodyMatchString: sub}

			if !matchBody(body, opts, nil) {
				return false, fmt.Errorf("matchBody returned false for body=%q containing substring=%q", body, sub)
			}
			return true, nil
		},
		genSafeBody(),
		genSubstring(),
		genSafeBody(),
	))

	// 14a-2: Substring absent → BodyMatch=false.
	properties.Property("substring absent from body implies BodyMatch=false", prop.ForAll(
		func(body string, sub string) (bool, error) {
			// Skip if the body accidentally contains the substring.
			if strings.Contains(body, sub) {
				return true, nil
			}

			opts := config.ProbeOptions{BodyMatchString: sub}

			if matchBody(body, opts, nil) {
				return false, fmt.Errorf("matchBody returned true for body=%q not containing substring=%q", body, sub)
			}
			return true, nil
		},
		genSafeBody(),
		genSubstring(),
	))

	properties.TestingRun(t)
}

// TestPropertyHTTPBodyRegexMatch verifies Property 14b:
// When body_match_regex is configured with a valid regex, matchBody returns
// true if and only if the regex matches the body.
//
// **Validates: Requirement 12.1, 12.3, 12.4**
func TestPropertyHTTPBodyRegexMatch(t *testing.T) {
	parameters := gopter.DefaultTestParameters()
	parameters.MinSuccessfulTests = 300
	parameters.MaxSize = 50
	properties := gopter.NewProperties(parameters)

	// 14b-1: Regex literal present in body → BodyMatch=true.
	// We use safe alphanumeric literals that are valid regex and match literally.
	properties.Property("regex literal present in body implies BodyMatch=true", prop.ForAll(
		func(prefix string, pattern string, suffix string) (bool, error) {
			body := prefix + pattern + suffix
			opts := config.ProbeOptions{BodyMatchRegex: regexp.QuoteMeta(pattern)}
			compiledRegex := regexp.MustCompile(opts.BodyMatchRegex)

			if !matchBody(body, opts, compiledRegex) {
				return false, fmt.Errorf("matchBody returned false for body=%q with regex=%q (pattern=%q present)",
					body, opts.BodyMatchRegex, pattern)
			}
			return true, nil
		},
		genSafeBody(),
		genSafeRegexLiteral(),
		genSafeBody(),
	))

	// 14b-2: Regex literal absent from body → BodyMatch=false.
	properties.Property("regex literal absent from body implies BodyMatch=false", prop.ForAll(
		func(body string, pattern string) (bool, error) {
			// Skip if the body accidentally contains the pattern.
			if strings.Contains(body, pattern) {
				return true, nil
			}

			opts := config.ProbeOptions{BodyMatchRegex: regexp.QuoteMeta(pattern)}
			compiledRegex := regexp.MustCompile(opts.BodyMatchRegex)

			if matchBody(body, opts, compiledRegex) {
				return false, fmt.Errorf("matchBody returned true for body=%q with regex=%q (pattern=%q absent)",
					body, opts.BodyMatchRegex, pattern)
			}
			return true, nil
		},
		genSafeBody(),
		genSafeRegexLiteral(),
	))

	// 14b-3: matchBody agrees with regexp.MatchString for valid patterns.
	properties.Property("matchBody agrees with regexp.MatchString for valid regex", prop.ForAll(
		func(body string, pattern string) (bool, error) {
			quotedPattern := regexp.QuoteMeta(pattern)
			opts := config.ProbeOptions{BodyMatchRegex: quotedPattern}

			re := regexp.MustCompile(quotedPattern)
			got := matchBody(body, opts, re)
			want := re.MatchString(body)

			if got != want {
				return false, fmt.Errorf("matchBody=%v but regexp.MatchString=%v for body=%q regex=%q",
					got, want, body, quotedPattern)
			}
			return true, nil
		},
		genSafeBody(),
		genSafeRegexLiteral(),
	))

	properties.TestingRun(t)
}

// TestPropertyHTTPBodyInvalidRegex verifies Property 14c:
// When body_match_regex contains an invalid regex pattern, matchBody always
// returns false regardless of the body content.
//
// **Validates: Requirement 12.1 (graceful handling of invalid regex)**
func TestPropertyHTTPBodyInvalidRegex(t *testing.T) {
	parameters := gopter.DefaultTestParameters()
	parameters.MinSuccessfulTests = 200
	parameters.MaxSize = 50
	properties := gopter.NewProperties(parameters)

	// Generate known-invalid regex patterns.
	genInvalidRegex := gen.OneConstOf(
		"[invalid(regex",
		"(?P<broken",
		"*unanchored",
		"[z-a]",
		"(unclosed",
		"\\",
	)

	properties.Property("invalid regex always yields BodyMatch=false", prop.ForAll(
		func(body string, invalidRegex string) (bool, error) {
			opts := config.ProbeOptions{BodyMatchRegex: invalidRegex}

			if matchBody(body, opts, nil) {
				return false, fmt.Errorf("matchBody returned true for invalid regex=%q body=%q", invalidRegex, body)
			}
			return true, nil
		},
		genSafeBody(),
		genInvalidRegex,
	))

	properties.TestingRun(t)
}

// TestPropertyHTTPBodyRegexPrecedence verifies Property 14d:
// When both body_match_regex and body_match_string are configured, the regex
// takes precedence — the substring pattern is ignored.
//
// **Validates: Requirement 12.1 (regex precedence over substring)**
func TestPropertyHTTPBodyRegexPrecedence(t *testing.T) {
	parameters := gopter.DefaultTestParameters()
	parameters.MinSuccessfulTests = 300
	parameters.MaxSize = 50
	properties := gopter.NewProperties(parameters)

	// When regex matches but substring does not, result should be true (regex wins).
	properties.Property("regex match + substring miss → BodyMatch=true (regex wins)", prop.ForAll(
		func(prefix string, regexLiteral string, suffix string, sub string) (bool, error) {
			body := prefix + regexLiteral + suffix

			// Skip if body accidentally contains the substring.
			if strings.Contains(body, sub) {
				return true, nil
			}

			opts := config.ProbeOptions{
				BodyMatchRegex:  regexp.QuoteMeta(regexLiteral),
				BodyMatchString: sub,
			}
			compiledRegex := regexp.MustCompile(opts.BodyMatchRegex)

			if !matchBody(body, opts, compiledRegex) {
				return false, fmt.Errorf("expected true (regex matches), got false; body=%q regex=%q sub=%q",
					body, opts.BodyMatchRegex, sub)
			}
			return true, nil
		},
		genSafeBody(),
		genSafeRegexLiteral(),
		genSafeBody(),
		genSubstring(),
	))

	// When regex does not match but substring does, result should be false (regex wins).
	properties.Property("regex miss + substring match → BodyMatch=false (regex wins)", prop.ForAll(
		func(prefix string, sub string, suffix string, regexLiteral string) (bool, error) {
			body := prefix + sub + suffix

			// Skip if body accidentally contains the regex literal.
			if strings.Contains(body, regexLiteral) {
				return true, nil
			}

			opts := config.ProbeOptions{
				BodyMatchRegex:  regexp.QuoteMeta(regexLiteral),
				BodyMatchString: sub,
			}
			compiledRegex := regexp.MustCompile(opts.BodyMatchRegex)

			if matchBody(body, opts, compiledRegex) {
				return false, fmt.Errorf("expected false (regex doesn't match), got true; body=%q regex=%q sub=%q",
					body, opts.BodyMatchRegex, sub)
			}
			return true, nil
		},
		genSafeBody(),
		genSubstring(),
		genSafeBody(),
		genSafeRegexLiteral(),
	))

	properties.TestingRun(t)
}

// TestPropertyHTTPBodyNeitherPattern verifies Property 14e:
// When neither body_match_regex nor body_match_string is configured,
// matchBody always returns false.
//
// **Validates: Requirement 12.1, 12.2 (no pattern → no match)**
func TestPropertyHTTPBodyNeitherPattern(t *testing.T) {
	parameters := gopter.DefaultTestParameters()
	parameters.MinSuccessfulTests = 200
	parameters.MaxSize = 50
	properties := gopter.NewProperties(parameters)

	properties.Property("no pattern configured always yields BodyMatch=false", prop.ForAll(
		func(body string) (bool, error) {
			opts := config.ProbeOptions{}

			if matchBody(body, opts, nil) {
				return false, fmt.Errorf("matchBody returned true with no pattern for body=%q", body)
			}
			return true, nil
		},
		genSafeBody(),
	))

	properties.TestingRun(t)
}
