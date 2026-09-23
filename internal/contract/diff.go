package contract

import (
	"strconv"
	"strings"
)

// Candidate represents an unmatched input leaf from a diff.
type Candidate struct {
	Path         string `json:"path"`
	Type         string `json:"type"`
	Len          int    `json:"len,omitempty"`
	Hash         string `json:"hash,omitempty"`
	Enum         string `json:"enum,omitempty"`
	Model        string `json:"model"`
	ClientFormat string `json:"clientFormat"`
	Direction    string `json:"direction"`
}

// Mapping maps an input path to an output path.
type Mapping struct {
	InPath  string `json:"inPath"`
	OutPath string `json:"outPath"`
}

// StaticNoiseList holds static field names that represent transport noise.
var StaticNoiseList = []string{
	"id",
	"created",
	"system_fingerprint",
	"date",
	"x-request-id",
}

// Diff compares input and output ShapeRecords with one-to-one matching.
// Request direction: input toolRequest, output request intact received.
// Response direction: input upstream answer, output toolResponse.
// Matching is one-to-one: an output leaf can match at most one input leaf.
// First matches on the same path, then on an approved mapping, then (for len >= 8) on the same hash.
// An enum leaf matches an output leaf with the same enum.
// Every unmatched input leaf is a candidate (at most 200).
func Diff(in, out ShapeRecord, mappings []Mapping, noise []string, model, clientFormat, direction string) ([]Candidate, []string) {
	// Gather all output leaves
	type outEntry struct {
		leaf    Leaf
		matched bool
	}
	var outLeaves []outEntry
	for _, rec := range out.Records {
		for _, leaf := range rec.Leaves {
			outLeaves = append(outLeaves, outEntry{leaf: leaf})
		}
	}

	mappingMap := make(map[string]string)
	for _, m := range mappings {
		mappingMap[m.InPath] = m.OutPath
	}

	noiseMap := make(map[string]bool)
	for _, n := range StaticNoiseList {
		noiseMap[strings.ToLower(n)] = true
	}
	for _, n := range noise {
		noiseMap[strings.ToLower(n)] = true
	}

	var candidates []Candidate

	// Iterate input leaves
	for _, rec := range in.Records {
		for _, inLeaf := range rec.Leaves {
			if inLeaf.Type == "cut" || inLeaf.Type == "invalid" {
				continue
			}

			// Check noise
			segs := strings.Split(inLeaf.Path, ".")
			lastSeg := segs[len(segs)-1]
			lastSeg = strings.TrimSuffix(lastSeg, "[]")
			if noiseMap[strings.ToLower(lastSeg)] {
				continue
			}

			matched := false

			// 1. Same path match
			for i := range outLeaves {
				if outLeaves[i].matched {
					continue
				}
				if outLeaves[i].leaf.Path == inLeaf.Path {
					if outLeaves[i].leaf.Type == "null" && inLeaf.Type != "null" {
						continue
					}
					if inLeaf.Enum != "" || outLeaves[i].leaf.Enum != "" {
						if inLeaf.Enum == outLeaves[i].leaf.Enum {
							outLeaves[i].matched = true
							matched = true
							break
						}
					} else {
						outLeaves[i].matched = true
						matched = true
						break
					}
				}
			}
			if matched {
				continue
			}

			// 2. Approved mapping match
			if mappedOut, ok := mappingMap[inLeaf.Path]; ok {
				for i := range outLeaves {
					if outLeaves[i].matched {
						continue
					}
					if outLeaves[i].leaf.Path == mappedOut {
						if outLeaves[i].leaf.Type == "null" && inLeaf.Type != "null" {
							continue
						}
						if inLeaf.Enum != "" || outLeaves[i].leaf.Enum != "" {
							if inLeaf.Enum == outLeaves[i].leaf.Enum {
								outLeaves[i].matched = true
								matched = true
								break
							}
						} else {
							outLeaves[i].matched = true
							matched = true
							break
						}
					}
				}
				if matched {
					continue
				}
			}

			// 3. Same hash match (for len >= 8 only)
			if inLeaf.Len >= 8 && inLeaf.Hash != "" {
				for i := range outLeaves {
					if outLeaves[i].matched {
						continue
					}
					if outLeaves[i].leaf.Len >= 8 && outLeaves[i].leaf.Hash == inLeaf.Hash {
						outLeaves[i].matched = true
						matched = true
						break
					}
				}
				if matched {
					continue
				}
			}

			// 4. Same enum match
			if inLeaf.Enum != "" {
				for i := range outLeaves {
					if outLeaves[i].matched {
						continue
					}
					if outLeaves[i].leaf.Enum == inLeaf.Enum {
						outLeaves[i].matched = true
						matched = true
						break
					}
				}
				if matched {
					continue
				}
			}

			// Unmatched input leaf is a candidate
			if len(candidates) < 200 {
				candidates = append(candidates, Candidate{
					Path:         inLeaf.Path,
					Type:         inLeaf.Type,
					Len:          inLeaf.Len,
					Hash:         inLeaf.Hash,
					Enum:         inLeaf.Enum,
					Model:        model,
					ClientFormat: clientFormat,
					Direction:    direction,
				})
			}
		}
	}

	// Collect up to 20 unmatched output paths for renamed options
	var unmatchedOutput []string
	for _, ol := range outLeaves {
		if !ol.matched && ol.leaf.Path != "" && ol.leaf.Type != "cut" && ol.leaf.Type != "invalid" {
			unmatchedOutput = append(unmatchedOutput, ol.leaf.Path)
			if len(unmatchedOutput) >= 20 {
				break
			}
		}
	}

	return candidates, unmatchedOutput
}

// CompareVersions compares two switcherVersions numerically by segment, then by UTC timestamp.
// e.g. "2.10.0+20260923T120000Z" vs "2.9.0+20260923T120000Z".
func CompareVersions(v1, v2 string) int {
	if v1 == v2 {
		return 0
	}
	p1 := strings.SplitN(v1, "+", 2)
	p2 := strings.SplitN(v2, "+", 2)

	segs1 := strings.Split(p1[0], ".")
	segs2 := strings.Split(p2[0], ".")

	maxLen := len(segs1)
	if len(segs2) > maxLen {
		maxLen = len(segs2)
	}

	for i := 0; i < maxLen; i++ {
		var n1, n2 int
		if i < len(segs1) {
			n1, _ = strconv.Atoi(segs1[i])
		}
		if i < len(segs2) {
			n2, _ = strconv.Atoi(segs2[i])
		}
		if n1 != n2 {
			if n1 > n2 {
				return 1
			}
			return -1
		}
	}

	var t1, t2 string
	if len(p1) > 1 {
		t1 = p1[1]
	}
	if len(p2) > 1 {
		t2 = p2[1]
	}
	if t1 < t2 {
		return -1
	} else if t1 > t2 {
		return 1
	}
	return 0
}
