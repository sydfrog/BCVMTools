// mksyso writes a Windows COFF .syso containing an application manifest
// (common-controls v6, DPI aware) so the walk GUI starts. Usage:
//
//	go run ./tools/mksyso app.manifest rsrc_windows_amd64.syso
package main

import (
	"bytes"
	"encoding/binary"
	"os"
)

func main() {
	man, err := os.ReadFile(os.Args[1])
	if err != nil {
		panic(err)
	}
	le := binary.LittleEndian
	w := func(b *bytes.Buffer, v ...any) {
		for _, x := range v {
			binary.Write(b, le, x)
		}
	}
	// resource section: type(24 RT_MANIFEST) -> id 1 -> lang 0x409 -> data entry -> data
	var rs bytes.Buffer
	w(&rs, uint32(0), uint32(0), uint16(0), uint16(0), uint16(0), uint16(1), uint32(24), uint32(0x80000000|24)) // 0..24
	w(&rs, uint32(0), uint32(0), uint16(0), uint16(0), uint16(0), uint16(1), uint32(1), uint32(0x80000000|48))  // 24..48
	w(&rs, uint32(0), uint32(0), uint16(0), uint16(0), uint16(0), uint16(1), uint32(0x409), uint32(72))         // 48..72
	dataOff := uint32(72 + 16)
	w(&rs, dataOff, uint32(len(man)), uint32(0), uint32(0)) // 72..88 (RVA relocated)
	rs.Write(man)
	for rs.Len()%4 != 0 {
		rs.WriteByte(0)
	}
	raw := rs.Bytes()

	var out bytes.Buffer
	relocOff := uint32(20 + 40 + len(raw))
	symOff := relocOff + 10
	w(&out, uint16(0x8664), uint16(1), uint32(0), symOff, uint32(1), uint16(0), uint16(0))
	name := [8]byte{'.', 'r', 's', 'r', 'c'}
	w(&out, name, uint32(0), uint32(0), uint32(len(raw)), uint32(60), relocOff, uint32(0), uint16(1), uint16(0), uint32(0x40000040))
	out.Write(raw)
	w(&out, uint32(72), uint32(0), uint16(3))                         // reloc: IMAGE_REL_AMD64_ADDR32NB at data entry
	w(&out, name, uint32(0), int16(1), uint16(0), uint8(3), uint8(0)) // section symbol
	w(&out, uint32(4))                                                // empty string table
	if err := os.WriteFile(os.Args[2], out.Bytes(), 0o644); err != nil {
		panic(err)
	}
}
