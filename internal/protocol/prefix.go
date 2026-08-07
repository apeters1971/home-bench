package protocol

import "hash/fnv"

// SelectPrefix picks a directory prefix from the hostname hash.
func SelectPrefix(hostname string, prefixes []string) string {
	if len(prefixes) == 0 {
		return ""
	}
	h := fnv.New32a()
	_, _ = h.Write([]byte(hostname))
	return prefixes[int(h.Sum32())%len(prefixes)]
}
