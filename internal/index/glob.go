package index

// matchGlob matches a key against a glob. A single * does not cross '/',
// while ** matches any sequence including '/'.
func matchGlob(pattern, name string) bool {
	memo := make(map[[2]int]bool)
	var match func(pi, ni int) bool
	match = func(pi, ni int) bool {
		key := [2]int{pi, ni}
		if value, ok := memo[key]; ok {
			return value
		}

		result := false
		switch {
		case pi == len(pattern):
			result = ni == len(name)
		case pattern[pi] == '*':
			j := pi
			for j < len(pattern) && pattern[j] == '*' {
				j++
			}
			if j-pi >= 2 {
				for k := ni; k <= len(name) && !result; k++ {
					result = match(j, k)
				}
				if !result && j < len(pattern) && pattern[j] == '/' {
					result = match(j+1, ni)
				}
			} else {
				for k := ni; ; k++ {
					if match(j, k) {
						result = true
						break
					}
					if k >= len(name) || name[k] == '/' {
						break
					}
				}
			}
		case pattern[pi] == '?':
			result = ni < len(name) && name[ni] != '/' && match(pi+1, ni+1)
		case pattern[pi] == '[':
			end := classEnd(pattern, pi)
			if end < 0 {
				result = ni < len(name) && name[ni] == '[' && match(pi+1, ni+1)
			} else {
				result = ni < len(name) && name[ni] != '/' &&
					classMatch(pattern[pi+1:end], name[ni]) && match(end+1, ni+1)
			}
		case pattern[pi] == '\\':
			if pi+1 < len(pattern) {
				result = ni < len(name) && name[ni] == pattern[pi+1] && match(pi+2, ni+1)
			} else {
				result = ni < len(name) && name[ni] == '\\' && match(pi+1, ni+1)
			}
		default:
			result = ni < len(name) && name[ni] == pattern[pi] && match(pi+1, ni+1)
		}

		memo[key] = result
		return result
	}
	return match(0, 0)
}

func classEnd(pattern string, start int) int {
	i := start + 1
	if i < len(pattern) && (pattern[i] == '!' || pattern[i] == '^') {
		i++
	}
	if i < len(pattern) && pattern[i] == ']' {
		i++
	}
	for i < len(pattern) {
		if pattern[i] == ']' {
			return i
		}
		i++
	}
	return -1
}

func classMatch(class string, c byte) bool {
	if class == "" {
		return false
	}
	negate := false
	if class[0] == '!' || class[0] == '^' {
		negate = true
		class = class[1:]
	}
	matched := false
	for i := 0; i < len(class); i++ {
		if i+2 < len(class) && class[i+1] == '-' {
			low, high := class[i], class[i+2]
			if low > high {
				low, high = high, low
			}
			if c >= low && c <= high {
				matched = true
			}
			i += 2
			continue
		}
		if class[i] == c {
			matched = true
		}
	}
	return matched != negate
}

// MatchGlob reports whether name matches pattern. A single * does not cross
// '/', while ** matches any sequence including '/'.
func MatchGlob(pattern, name string) bool { return matchGlob(pattern, name) }
