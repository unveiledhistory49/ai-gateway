package policy

import "regexp"

var ssnRegex = regexp.MustCompile(`\b([0-9]{3})-([0-9]{2})-([0-9]{4})\b`)

// ValidateSSN validates the area, group, and serial number rules of US Social Security Numbers.
func ValidateSSN(area, group, serial string) bool {
	if len(area) != 3 || len(group) != 2 || len(serial) != 4 {
		return false
	}

	// Area number (first 3 digits): cannot be 000, 666, or 900-999
	if area == "000" || area == "666" || area >= "900" {
		return false
	}

	// Group number (middle 2 digits): cannot be 00
	if group == "00" {
		return false
	}

	// Serial number (last 4 digits): cannot be 0000
	if serial == "0000" {
		return false
	}

	return true
}

// FindSSNSpans returns [start, end] byte indices of valid US SSNs within text.
func FindSSNSpans(text string) [][]int {
	matches := ssnRegex.FindAllStringSubmatchIndex(text, -1)
	var validSpans [][]int
	for _, m := range matches {
		// m[0], m[1] is the entire match
		// m[2], m[3] is area
		// m[4], m[5] is group
		// m[6], m[7] is serial
		area := text[m[2]:m[3]]
		group := text[m[4]:m[5]]
		serial := text[m[6]:m[7]]

		if ValidateSSN(area, group, serial) {
			validSpans = append(validSpans, []int{m[0], m[1]})
		}
	}
	return validSpans
}
