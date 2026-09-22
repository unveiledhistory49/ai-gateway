package policy

// ValidateLuhn verifies whether a credit card number passes the Luhn (Mod 10) checksum.
func ValidateLuhn(number string) bool {
	// Strip delimiters (hyphens, spaces)
	digits := make([]int, 0, len(number))
	for _, ch := range number {
		if ch >= '0' && ch <= '9' {
			digits = append(digits, int(ch-'0'))
		} else if ch == '-' || ch == ' ' {
			continue
		} else {
			// Invalid character
			return false
		}
	}

	// Standard credit card numbers have between 13 and 19 digits
	if len(digits) < 13 || len(digits) > 19 {
		return false
	}

	sum := 0
	double := false

	// Iterate right-to-left
	for i := len(digits) - 1; i >= 0; i-- {
		d := digits[i]
		if double {
			d *= 2
			if d > 9 {
				d -= 9
			}
		}
		sum += d
		double = !double
	}

	return sum%10 == 0
}
