package transcript

// A minimal protobuf wire-format reader for the few fields Relay reads out of
// agy's conversation database. The schema is agy's own and undocumented; the
// field numbers below were confirmed live against agy 1.2.12/1.2.13 by
// decoding real databases, and everything that does not match is ignored,
// never fatal: an unknown shape yields "no value", not an error.

// pbFields calls fn for every top-level field of msg until fn returns false.
// For length-delimited fields (wire type 2) data is the payload; for varints
// (wire type 0) num is the value.
func pbFields(msg []byte, fn func(field int, wire int, num uint64, data []byte) bool) {
	for i := 0; i < len(msg); {
		key, n := pbVarint(msg[i:])
		if n <= 0 {
			return
		}
		i += n
		field, wire := int(key>>3), int(key&7)
		if field == 0 {
			return
		}
		switch wire {
		case 0:
			v, n := pbVarint(msg[i:])
			if n <= 0 {
				return
			}
			i += n
			if !fn(field, wire, v, nil) {
				return
			}
		case 1:
			if i+8 > len(msg) {
				return
			}
			i += 8
		case 2:
			l, n := pbVarint(msg[i:])
			if n <= 0 || l > uint64(len(msg)-i-n) {
				return
			}
			i += n
			data := msg[i : i+int(l)]
			i += int(l)
			if !fn(field, wire, 0, data) {
				return
			}
		case 5:
			if i+4 > len(msg) {
				return
			}
			i += 4
		default:
			return
		}
	}
}

func pbVarint(b []byte) (uint64, int) {
	var v uint64
	for i := 0; i < len(b) && i < 10; i++ {
		v |= uint64(b[i]&0x7f) << (7 * i)
		if b[i] < 0x80 {
			return v, i + 1
		}
	}
	return 0, 0
}

// pbString returns the first length-delimited value at the nested field path.
func pbString(msg []byte, path ...int) (string, bool) {
	if len(path) == 0 {
		return "", false
	}
	var out []byte
	found := false
	pbFields(msg, func(field, wire int, _ uint64, data []byte) bool {
		if field != path[0] || wire != 2 {
			return true
		}
		if len(path) == 1 {
			out, found = data, true
			return false
		}
		if s, ok := pbString(data, path[1:]...); ok {
			out, found = []byte(s), true
			return false
		}
		return true
	})
	return string(out), found
}

// pbUint returns the first varint at the nested field path.
func pbUint(msg []byte, path ...int) (uint64, bool) {
	if len(path) == 0 {
		return 0, false
	}
	var out uint64
	found := false
	pbFields(msg, func(field, wire int, num uint64, data []byte) bool {
		if field != path[0] {
			return true
		}
		if len(path) == 1 {
			if wire == 0 {
				out, found = num, true
				return false
			}
			return true
		}
		if wire == 2 {
			if v, ok := pbUint(data, path[1:]...); ok {
				out, found = v, true
				return false
			}
		}
		return true
	})
	return out, found
}
