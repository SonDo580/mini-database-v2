package db

func assert(cond bool) {
	if !cond {
		panic("assertion failure")
	}
}
