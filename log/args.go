package log

import (
	"fmt"
	"strings"
)

// UserTagArguments returns a new slice with the values for the flags given in flagsToTag surrounded by the <ud></ud>
// tags.
func UserTagArguments(args, flagsToTag []string) []string {
	ret := make([]string, len(args))
	copy(ret, args)

	for i := 0; i < len(ret); i++ {
		if flagMatches(ret[i], flagsToTag) {
			i++

			ret[i] = fmt.Sprintf("<ud>%s</ud>", ret[i])
		}
	}

	return ret
}

// MaskArguments returns a new slice with the values of the flags given in flagsToMask replaced by a fix number of *.
func MaskArguments(args, flagsToMask []string) []string {
	ret := make([]string, len(args))
	copy(ret, args)

	for i := 0; i < len(ret); i++ {
		// Only mask if it matches the flagsToMask and if it has a value afterwards.
		if flagMatches(ret[i], flagsToMask) && i+1 < len(ret) && !strings.HasPrefix(ret[i+1], "-") {
			i++

			ret[i] = "*****" // Mask with fix length to avoid revealing any details about the string.
		}
	}

	return ret
}

// MaskAndUserTagArguments is a convenient way of calling both UserTagArguments and MaskArguments on the given data. It
// will return the resulting string slice joined with a space between element for easy logging.
func MaskAndUserTagArguments(args, flagsToTag, flagsToMask []string) string {
	return strings.TrimSpace(strings.Join(MaskArguments(UserTagArguments(args, flagsToTag), flagsToMask), " "))
}

func flagMatches(flag string, flags []string) bool {
	for _, prefix := range flags {
		if strings.HasPrefix(flag, prefix) {
			return true
		}
	}

	return false
}
