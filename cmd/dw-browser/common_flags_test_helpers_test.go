package main

// Existing pure parser fixtures keep their two-value helper shape while testing
// the actual nonexiting parser. No production caller has this adapter.
func parseCommonFlags(args []string, cmd string) ([]string, commonFlags) {
	pos, f, e := parseCommonFlagsE(args, cmd)
	if e != nil {
		panic(e)
	}
	return pos, f
}
