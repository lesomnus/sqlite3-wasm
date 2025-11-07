package assert

func NoErr(err error) {
	if err == nil {
		return
	}

	panic(err.Error())
}

func X(st bool, msg ...string) {
	if st {
		return
	}

	panic(msg)
}
