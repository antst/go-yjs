package crdt

// define the mask to get the low n bits of a byte.
const (
	bits0 = 0
	bits1 = 1   // low 1 bit
	bits2 = 3   // low 2 bits
	bits3 = 7   // low 3 bits
	bits4 = 15  // low 4 bits
	bits5 = 31  // low 5 bits
	bits6 = 63  // low 6 bits
	bits7 = 127 // low 7 bits
	bits8 = 255 // low 8 bits

	// bits31 is the largest magnitude lib0's writeAny encodes as a varint integer
	// (tag 125); numbers above it fall through to the float32/float64 cascade.
	bits31 = 0x7FFFFFFF
)

// define the mask to get the specific bit of a byte.
const (
	bit1 = 1   // first bit
	bit2 = 2   // second bit
	bit3 = 4   // third bit
	bit4 = 8   // fourth bit
	bit5 = 16  // fifth bit
	bit6 = 32  // sixth bit
	bit7 = 64  // seventh bit
	bit8 = 128 // eighth bit
)

const (
	keywordUndefined = "undefined"
)

// RefContent define reference content type
const (
	refGC             = iota // 0 GC is not ItemContent
	refContentDeleted        // 1
	refContentJSON           // 2
	refContentBinary         // 3
	refContentString         // 4
	refContentEmbed          // 5
	refContentFormat         // 6
	refContentType           // 7
	refContentAny            // 8
	refContentDoc            // 9
	refSkip                  // 10 Skip is not ItemContent
)

// RefID define reference id
const (
	yArrayRefID = iota
	yMapRefID
	yTextRefID
	yXMLElementRefID
	yXMLFragmentRefID
	yXMLHookRefID
	yXMLTextRefID
)
