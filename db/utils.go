package db

func assert(cond bool) {
	if !cond {
		panic("assertion failure")
	}
}

// satisfy Go's sort.Interface
type sortIF struct {
	len  int
	less func(i, j int) bool
	swap func(i, j int)
}

func (self sortIF) Len() int {
	return self.len
}
func (self sortIF) Less(i, j int) bool {
	return self.less(i, j)
}
func (self sortIF) Swap(i, j int) {
	self.swap(i, j)
}

//   - "finalization mix" function used in MurmurHash3 to ensure
//     the final hash bits are well-distributed and have high entropy.
//   - in short: small change in input produces drastically different output.
//   - why it works: TODO
func fmix32(h uint32) uint32 {
	h ^= h >> 16
	h *= 0x85ebca6b
	h ^= h >> 13
	h *= 0xc2b2ae35
	h ^= h >> 16
	return h
}
