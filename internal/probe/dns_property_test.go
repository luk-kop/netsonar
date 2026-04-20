package probe

import (
	"fmt"
	"reflect"
	"sort"
	"strings"
	"testing"

	"github.com/leanovate/gopter"
	"github.com/leanovate/gopter/gen"
	"github.com/leanovate/gopter/prop"
)

// genIPv4 generates a random IPv4 address string.
func genIPv4() gopter.Gen {
	return gopter.CombineGens(
		gen.IntRange(1, 254),
		gen.IntRange(0, 255),
		gen.IntRange(0, 255),
		gen.IntRange(1, 254),
	).Map(func(vals []interface{}) string {
		return fmt.Sprintf("%d.%d.%d.%d",
			vals[0].(int), vals[1].(int), vals[2].(int), vals[3].(int))
	})
}

// genDomainLabel generates a short lowercase domain label (e.g. "abc", "test").
func genDomainLabel() gopter.Gen {
	return gen.IntRange(2, 8).FlatMap(func(v interface{}) gopter.Gen {
		n := v.(int)
		return gen.SliceOfN(n, gen.AlphaChar()).Map(func(chars []rune) string {
			return strings.ToLower(string(chars))
		})
	}, reflect.TypeOf(""))
}

// genDomain generates a plausible domain name (e.g. "foo.example.com").
func genDomain() gopter.Gen {
	return gopter.CombineGens(
		genDomainLabel(),
		genDomainLabel(),
	).Map(func(vals []interface{}) string {
		return vals[0].(string) + "." + vals[1].(string) + ".com"
	})
}

// genDNSRecord generates either an IPv4 address or a domain name, covering
// both A-record and CNAME-record result shapes.
func genDNSRecord() gopter.Gen {
	return gen.Bool().FlatMap(func(v interface{}) gopter.Gen {
		if v.(bool) {
			return genIPv4()
		}
		return genDomain()
	}, reflect.TypeOf(""))
}

// genDNSRecordSlice generates a non-empty slice of 1–6 unique DNS records.
func genDNSRecordSlice() gopter.Gen {
	return gen.SliceOfN(6, genDNSRecord()).
		Map(func(records []string) []string {
			seen := make(map[string]bool)
			unique := make([]string, 0, len(records))
			for _, r := range records {
				lower := strings.ToLower(r)
				if !seen[lower] {
					seen[lower] = true
					unique = append(unique, r)
				}
			}
			return unique
		}).
		SuchThat(func(records []string) bool {
			return len(records) >= 1
		})
}

// TestPropertyDNSExpectedResultValidation verifies Property 13:
// DNS expected result validation correctness. The matchExpected function
// must satisfy the following properties:
//
//   - 13a (Reflexivity): Any record set matches itself.
//   - 13b (Order independence): Shuffling the got or want slices does not
//     change the match result.
//   - 13c (Case insensitivity): Changing the case of records does not
//     change the match result.
//   - 13d (Trailing dot normalization): Adding or removing trailing dots
//     does not change the match result.
//   - 13e (Whitespace normalization): Adding leading/trailing whitespace
//     does not change the match result.
//   - 13f (Strict set equality): Adding an extra record to one side
//     causes a mismatch.
//   - 13g (Substitution detection): Replacing a record with a different
//     value causes a mismatch.
//
// **Validates: Requirements 10.2, 15.1**
func TestPropertyDNSExpectedResultValidation(t *testing.T) {
	parameters := gopter.DefaultTestParameters()
	parameters.MinSuccessfulTests = 300
	parameters.MaxSize = 50
	properties := gopter.NewProperties(parameters)

	// --- Property 13a: Reflexivity ---
	properties.Property("matchExpected is reflexive: records match themselves", prop.ForAll(
		func(records []string) (bool, error) {
			got := make([]string, len(records))
			copy(got, records)
			want := make([]string, len(records))
			copy(want, records)

			if !matchExpected(got, want) {
				return false, fmt.Errorf("reflexivity failed: records=%v did not match themselves", records)
			}
			return true, nil
		},
		genDNSRecordSlice(),
	))

	// --- Property 13b: Order independence ---
	properties.Property("matchExpected is order-independent", prop.ForAll(
		func(records []string) (bool, error) {
			// Create a reversed copy.
			reversed := make([]string, len(records))
			for i, r := range records {
				reversed[len(records)-1-i] = r
			}

			if !matchExpected(records, reversed) {
				return false, fmt.Errorf("order independence failed: %v vs reversed %v", records, reversed)
			}

			// Also test with a sorted copy.
			sorted := make([]string, len(records))
			copy(sorted, records)
			sort.Strings(sorted)

			if !matchExpected(records, sorted) {
				return false, fmt.Errorf("order independence failed: %v vs sorted %v", records, sorted)
			}

			return true, nil
		},
		genDNSRecordSlice(),
	))

	// --- Property 13c: Case insensitivity ---
	properties.Property("matchExpected is case-insensitive", prop.ForAll(
		func(records []string) (bool, error) {
			upper := make([]string, len(records))
			for i, r := range records {
				upper[i] = strings.ToUpper(r)
			}

			if !matchExpected(records, upper) {
				return false, fmt.Errorf("case insensitivity failed: %v vs %v", records, upper)
			}

			lower := make([]string, len(records))
			for i, r := range records {
				lower[i] = strings.ToLower(r)
			}

			if !matchExpected(records, lower) {
				return false, fmt.Errorf("case insensitivity failed: %v vs %v", records, lower)
			}

			return true, nil
		},
		genDNSRecordSlice(),
	))

	// --- Property 13d: Trailing dot normalization ---
	properties.Property("matchExpected normalizes trailing dots", prop.ForAll(
		func(records []string) (bool, error) {
			withDots := make([]string, len(records))
			for i, r := range records {
				withDots[i] = r + "."
			}

			if !matchExpected(records, withDots) {
				return false, fmt.Errorf("trailing dot normalization failed: %v vs %v", records, withDots)
			}
			return true, nil
		},
		genDNSRecordSlice(),
	))

	// --- Property 13e: Whitespace normalization ---
	properties.Property("matchExpected normalizes leading/trailing whitespace", prop.ForAll(
		func(records []string) (bool, error) {
			padded := make([]string, len(records))
			for i, r := range records {
				padded[i] = "  " + r + "  "
			}

			if !matchExpected(records, padded) {
				return false, fmt.Errorf("whitespace normalization failed: %v vs %v", records, padded)
			}
			return true, nil
		},
		genDNSRecordSlice(),
	))

	// --- Property 13f: Strict set equality (extra record → mismatch) ---
	properties.Property("matchExpected rejects when got has extra record", prop.ForAll(
		func(records []string, extra string) (bool, error) {
			// Ensure extra is not already in the set.
			for _, r := range records {
				if strings.EqualFold(
					strings.TrimSuffix(strings.TrimSpace(r), "."),
					strings.TrimSuffix(strings.TrimSpace(extra), "."),
				) {
					// Skip this case — extra is a duplicate.
					return true, nil
				}
			}

			extended := append(append([]string{}, records...), extra)

			if matchExpected(extended, records) {
				return false, fmt.Errorf("strict equality failed: %v (with extra %q) matched %v",
					extended, extra, records)
			}
			return true, nil
		},
		genDNSRecordSlice(),
		genDNSRecord(),
	))

	// --- Property 13g: Substitution detection ---
	properties.Property("matchExpected detects record substitution", prop.ForAll(
		func(records []string, replacement string) (bool, error) {
			if len(records) == 0 {
				return true, nil
			}

			// Normalize for comparison.
			norm := func(s string) string {
				return strings.TrimSuffix(strings.ToLower(strings.TrimSpace(s)), ".")
			}

			// Check if replacement is already equivalent to the first record.
			if norm(replacement) == norm(records[0]) {
				return true, nil
			}

			// Substitute the first record.
			modified := make([]string, len(records))
			copy(modified, records)
			modified[0] = replacement

			if matchExpected(modified, records) {
				return false, fmt.Errorf("substitution detection failed: replaced %q with %q in %v, still matched %v",
					records[0], replacement, modified, records)
			}
			return true, nil
		},
		genDNSRecordSlice(),
		genDNSRecord(),
	))

	properties.TestingRun(t)
}

// TestPropertyDNSEmptySlicesBehavior verifies edge-case behavior:
// two empty slices always match, and an empty slice never matches a non-empty one.
func TestPropertyDNSEmptySlicesBehavior(t *testing.T) {
	parameters := gopter.DefaultTestParameters()
	parameters.MinSuccessfulTests = 100
	properties := gopter.NewProperties(parameters)

	properties.Property("empty got vs non-empty want never matches", prop.ForAll(
		func(records []string) (bool, error) {
			if matchExpected([]string{}, records) {
				return false, fmt.Errorf("empty got matched non-empty want: %v", records)
			}
			return true, nil
		},
		genDNSRecordSlice(),
	))

	properties.Property("non-empty got vs empty want never matches", prop.ForAll(
		func(records []string) (bool, error) {
			if matchExpected(records, []string{}) {
				return false, fmt.Errorf("non-empty got %v matched empty want", records)
			}
			return true, nil
		},
		genDNSRecordSlice(),
	))

	properties.TestingRun(t)
}
