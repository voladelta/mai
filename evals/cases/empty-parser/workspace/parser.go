package parser

import "strings"

func ParsePairs(input string) map[string]string {
	lines := strings.Split(input, "\n")
	first := strings.SplitN(lines[0], "=", 2)
	result := map[string]string{first[0]: first[1]}

	for _, line := range lines[1:] {
		if line == "" {
			continue
		}
		parts := strings.SplitN(line, "=", 2)
		result[parts[0]] = parts[1]
	}
	return result
}
