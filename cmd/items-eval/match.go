package main

import "github.com/ZacxDev/muster-meetings/internal/items"

// Matching emitted items to labels, one to one (operator's decision on #16).
//
// An item CAN match a label when they cite a segment in common (no wording
// is compared: no model judges a model). Of those possible pairs, the score
// takes a MAXIMUM matching: as many pairs as there can be, each item in at
// most one and each label in at most one. Among the maximum matchings it
// prefers, in order: more pairs that share a segment that is the label's
// OWN (one no other label of the meeting cites; a recap restating several
// items is no label's own), then more shared segments, then items paired
// with labels in their order (the earlier item with the earlier label), so
// the same input always gives the same pairs.
//
// An item left unpaired although it could match a label (every label it
// could match went to another item) is a DUPLICATE, charged to the label it
// would have preferred; an item that could match no label is a false
// positive. Both count against precision.

// matching is one meeting's pairs: label[i] is item i's label (-1: none),
// dupOf[i] the label an unpaired item i duplicates (-1: not a duplicate),
// and adj[i] every label item i could match.
type matching struct {
	label, dupOf []int
	adj          [][]int
}

// match pairs a meeting's emitted items with its labels.
func match(emitted []items.Item, labels []Label) matching {
	nI, nL := len(emitted), len(labels)
	own := ownSegments(labels)
	shared := make([][]int, nI) // segments item i and label j share
	ownHit := make([][]bool, nI)
	adj := make([][]int, nI)
	maxShared := 0
	for i, e := range emitted {
		shared[i], ownHit[i] = make([]int, nL), make([]bool, nL)
		for j, l := range labels {
			for _, a := range uniqIDs(e.Segments) {
				for _, b := range uniqIDs(l.Segments) {
					if a == b {
						shared[i][j]++
						ownHit[i][j] = ownHit[i][j] || own[j][a]
					}
				}
			}
			if shared[i][j] > 0 {
				adj[i] = append(adj[i], j)
				maxShared = max(maxShared, shared[i][j])
			}
		}
	}
	mt := matching{label: make([]int, nI), dupOf: make([]int, nI), adj: adj}
	for i := range mt.label {
		mt.label[i], mt.dupOf[i] = -1, -1
	}
	if nI == 0 || nL == 0 {
		return mt
	}
	// One weight per possible pair, in four tiers each larger than every
	// sum of the tiers below it over a whole matching, so a maximum-weight
	// matching is a maximum matching first, then the preferences in order.
	n := int64(min(nI, nL))
	tie := func(i, j int) int64 { return int64(nI-i) * int64(nL-j) } // earlier with earlier
	cTie := n*int64(nI)*int64(nL) + 1
	cShared := cTie
	cOwn := n*(int64(maxShared)*cShared+cTie) + 1
	cPair := n*(cOwn+int64(maxShared)*cShared+cTie) + 1
	w := make([][]int64, nI)
	for i := range w {
		w[i] = make([]int64, nL)
		for _, j := range adj[i] {
			o := int64(0)
			if ownHit[i][j] {
				o = 1
			}
			w[i][j] = cPair + o*cOwn + int64(shared[i][j])*cShared + tie(i, j)
		}
	}
	pairs := assign(w)
	for i, j := range pairs {
		if j >= 0 && w[i][j] > 0 {
			mt.label[i] = j
		}
	}
	for i := range emitted {
		if mt.label[i] < 0 && len(adj[i]) > 0 {
			best := adj[i][0]
			for _, j := range adj[i][1:] {
				if better(ownHit[i][j], shared[i][j], ownHit[i][best], shared[i][best]) {
					best = j
				}
			}
			mt.dupOf[i] = best
		}
	}
	return mt
}

// better reports a pair (own, shared) preferred to (own0, shared0); equal
// is not better, so the earlier label wins a tie.
func better(own bool, shared int, own0 bool, shared0 int) bool {
	if own != own0 {
		return own
	}
	return shared > shared0
}

// ownSegments is, per label, the segments no other label of the meeting
// cites.
func ownSegments(labels []Label) []map[int64]bool {
	n := map[int64]int{}
	for _, l := range labels {
		for _, id := range uniqIDs(l.Segments) {
			n[id]++
		}
	}
	out := make([]map[int64]bool, len(labels))
	for i, l := range labels {
		out[i] = map[int64]bool{}
		for _, id := range l.Segments {
			if n[id] == 1 {
				out[i][id] = true
			}
		}
	}
	return out
}

func uniqIDs(ids []int64) []int64 {
	seen := map[int64]bool{}
	var out []int64
	for _, id := range ids {
		if !seen[id] {
			seen[id] = true
			out = append(out, id)
		}
	}
	return out
}

// assign is a maximum-weight assignment of rows to columns of w (rows ×
// cols, weights ≥ 0): the Hungarian algorithm on the square matrix padded
// with zeros, as a minimum-cost assignment of (max − w). It answers each
// row's column, -1 for a row assigned to padding.
func assign(w [][]int64) []int {
	rows, cols := len(w), len(w[0])
	n := max(rows, cols)
	var top int64
	for _, r := range w {
		for _, x := range r {
			top = max(top, x)
		}
	}
	cost := func(i, j int) int64 { // 0-based
		if i < rows && j < cols {
			return top - w[i][j]
		}
		return top
	}
	const inf = int64(1) << 62
	u, v := make([]int64, n+1), make([]int64, n+1)
	p, way := make([]int, n+1), make([]int, n+1)
	for i := 1; i <= n; i++ {
		p[0] = i
		j0 := 0
		minv := make([]int64, n+1)
		used := make([]bool, n+1)
		for j := range minv {
			minv[j] = inf
		}
		for {
			used[j0] = true
			i0, delta, j1 := p[j0], inf, 0
			for j := 1; j <= n; j++ {
				if used[j] {
					continue
				}
				if cur := cost(i0-1, j-1) - u[i0] - v[j]; cur < minv[j] {
					minv[j], way[j] = cur, j0
				}
				if minv[j] < delta {
					delta, j1 = minv[j], j
				}
			}
			for j := 0; j <= n; j++ {
				if used[j] {
					u[p[j]] += delta
					v[j] -= delta
				} else {
					minv[j] -= delta
				}
			}
			j0 = j1
			if p[j0] == 0 {
				break
			}
		}
		for j0 != 0 {
			j1 := way[j0]
			p[j0] = p[j1]
			j0 = j1
		}
	}
	out := make([]int, rows)
	for i := range out {
		out[i] = -1
	}
	for j := 1; j <= n; j++ {
		if i := p[j] - 1; i >= 0 && i < rows && j-1 < cols {
			out[i] = j - 1
		}
	}
	return out
}
