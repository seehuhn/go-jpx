// Copyright 2026 Jochen Voss <voss@seehuhn.de>
// SPDX-License-Identifier: BSD-3-Clause

// Package jpx reads and writes JPEG 2000 images.
//
// The package implements the core coding system (JPEG 2000 Part 1)
// together with the JP2 file format and the parts of the JPX format that
// PDF files use.  Input may be a JP2/JPX file or a bare codestream.
//
// Decoding returns the image samples as they are stored in the
// codestream: one plane of signed 32-bit integers per component, at the
// component's native bit depth and signedness (components deeper than
// MaxPrecision bits are returned at MaxPrecision bits of precision), with
// the inverse wavelet and component transforms applied but without any
// colour conversion or palette lookup.  The header reports everything
// the file says about the interpretation of the samples (colour
// specifications, channel definitions, palette), and helper functions
// perform the common conversions on request.
//
// The decoder is designed for untrusted input: it is written not to
// panic, bounds its memory use and work through a [membudget.Budget],
// and tolerates truncated or partially corrupt codestreams by returning
// the image decoded so far.
package jpx

import (
	"seehuhn.de/go/membudget"
)

// Component describes one image component as coded in the codestream.
type Component struct {
	// Depth is the number of bits per sample.  Decode and Encode accept
	// 1 to MaxDepth; ReadHeader also reports deeper components.
	Depth  int
	Signed bool // samples are two's complement when true, unsigned otherwise

	// Truncation is the number of low-order bits of each sample that the
	// plane does not hold.  It is max(0, Depth-MaxPrecision): the plane
	// holds the sample values divided by 2^Truncation, with Depth -
	// Truncation significant bits.  The decoder computes these values
	// from the coefficients without their lowest bits, so that a value
	// may differ slightly from the exact quotient of a sample that had
	// non-zero low bits.  Encode accepts only this value of Truncation.
	Truncation int

	// DX and DY are the horizontal and vertical sub-sampling factors:
	// the component has one sample for every DX×DY points of the
	// reference grid.
	DX, DY int

	// Width and Height give the number of samples of the component at
	// full resolution.  The samples cover component coordinates
	// [ceil(XOffset/DX), ceil((XOffset+Width)/DX)) horizontally, and
	// correspondingly vertically, where XOffset and Width are those of
	// the image.
	Width, Height int
}

// ChannelKind classifies a channel of the decoded image.
type ChannelKind int

// The channel kinds of the JP2 channel definition box.
const (
	ChannelColor                ChannelKind = iota // ordinary colour channel
	ChannelOpacity                                 // opacity (alpha)
	ChannelPremultipliedOpacity                    // opacity, colour channels premultiplied
	ChannelUnknown                                 // any other channel type
)

// Channel describes how one channel of the image is to be interpreted.
// Channels are listed in channel order.  Without a palette, channel i
// takes its samples from component i; with a palette, the component
// mapping box determines the source of each channel.  Component and
// PaletteColumn always refer to existing components and columns: a
// mapping box with invalid references is ignored together with the
// palette, and channel definitions for channels that do not exist are
// dropped.
type Channel struct {
	// Component is the index of the component providing the samples.
	Component int

	// PaletteColumn is the palette column the component samples index
	// into, or -1 if the samples are used directly.
	PaletteColumn int

	// Kind classifies the channel; Type is the raw channel type value
	// from the channel definition box (0 colour, 1 opacity, 2
	// premultiplied opacity, 65535 unspecified), or 0 when synthesised.
	Kind ChannelKind
	Type int

	// Assoc gives the colour channel this channel is associated with:
	// 0 means the whole image, n ≥ 1 means colour channel n (counted
	// from 1), and 65535 means no association.  For colour channels it
	// is the channel's position in the colour space.
	Assoc int
}

// ColorSpec is one colour specification from the JP2 header.
type ColorSpec struct {
	// Method is 1 for an enumerated colour space, 2 for a restricted ICC
	// profile, 3 for any ICC profile and 4 for a vendor colour space.
	// It is never 0: a colour specification box with the reserved
	// method 0 is dropped when reading.
	Method int

	Precedence int
	Approx     int

	// Enumerated is the enumerated colour space for Method 1, for example
	// 16 (sRGB), 17 (greyscale), 18 (sYCC) or 12 (CMYK).
	Enumerated int

	// EnumParams holds any parameters following the enumerated colour
	// space value.
	EnumParams []byte

	// ICC holds the raw ICC profile for Methods 2 and 3.
	ICC []byte

	// Vendor holds the raw payload of a vendor colour specification.
	Vendor []byte
}

// Palette holds the palette (pclr box) of an image.  Entries[i][j] is
// the value of column j of palette entry i.
type Palette struct {
	Entries [][]int32
	Columns []PaletteColumn
}

// PaletteColumn describes the values of one palette column.  Depth is
// between 1 and 38.  Truncation is max(0, Depth-31): the entries of the
// column hold the values divided by 2^Truncation, rounded towards
// negative infinity, so that they fit an int32.  Encode accepts only this
// value of Truncation and writes the entries multiplied by
// 2^Truncation.
type PaletteColumn struct {
	Depth      int
	Signed     bool
	Truncation int
}

// Header describes a JPEG 2000 image without its sample data.
type Header struct {
	// XOffset and YOffset give the position of the image area on the
	// reference grid: the image covers [XOffset, XOffset+Width) ×
	// [YOffset, YOffset+Height).
	XOffset, YOffset int

	// Width and Height give the image size on the reference grid, after
	// the image offset.  Components with DX or DY > 1 have fewer samples.
	Width, Height int

	Components []Component

	// Channels lists the channels of the image.  When the file has no
	// channel definition box, one colour channel per component (or per
	// palette mapping entry) is synthesised.
	Channels []Channel

	// ColorSpecs holds every colour specification box in file order.
	// It is empty for a bare codestream.
	ColorSpecs []ColorSpec

	// Palette is the palette of the image, or nil.
	Palette *Palette

	// Resolutions is the number of resolution levels available for
	// reduced decoding: Options.Reduce may be at most Resolutions-1.
	Resolutions int

	// Wrapped reports whether the data had a JP2/JPX wrapper (true) or
	// was a bare codestream (false).
	Wrapped bool

	// Tiles is the number of tiles of the codestream.
	Tiles int
}

// Plane holds the samples of one component, with at most MaxPrecision
// significant bits; see Component.Truncation.
type Plane struct {
	Width, Height int
	Pix           []int32 // Width*Height samples in row-major order
}

// At returns the sample at (x, y).
func (p *Plane) At(x, y int) int32 { return p.Pix[y*p.Width+x] }

// Image is a decoded JPEG 2000 image.
type Image struct {
	// Header describes the image at full resolution, whatever the
	// reduction.
	Header

	// Reduce is the number of resolution levels that were discarded.
	Reduce int

	// Planes holds one plane per component, in component order.  The
	// samples are at the component's native depth and signedness,
	// divided by 2^Truncation for components deeper than MaxPrecision
	// bits.  With Reduce > 0 the planes are correspondingly smaller.
	Planes []Plane
}

// Options controls decoding.
type Options struct {
	// Budget bounds the working memory, including the copy of the input
	// and the decoded planes, and the decoding work.  Every allocation
	// whose size depends on the input is charged before it is made, and
	// decoding work is charged at one byte per coefficient per coding
	// pass, so that a budget bounds the CPU time as well, at a few
	// nanoseconds per charged byte.  Exceeding the budget aborts decoding
	// with membudget.ErrExceeded.  A nil Budget imposes no limit; callers
	// decoding untrusted input must supply one.
	Budget *membudget.Budget

	// Reduce discards this many resolution levels: the result has
	// roughly 1/2^Reduce of the width and height of the full image.
	Reduce int

	// Workers is the number of goroutines used for decoding: they work on
	// whole tiles when the image has enough of them, and on the
	// code-blocks of each tile otherwise.  Zero or one decodes
	// sequentially.  With several workers, which of several errors is
	// reported may vary between runs.
	Workers int
}

// MaxDepth is the largest component depth that Decode and Encode accept;
// ReadHeader also reports deeper components.  It is the largest depth for
// which Encode can write every decoded image back exactly: the samples of
// deeper components need more magnitude bit-planes than a codestream can
// signal for some sub-bands.
const MaxDepth = 34

// MaxPrecision is the largest number of significant bits a Plane holds.
// Components deeper than MaxPrecision bits are held without their lowest
// bits, as described for Component.Truncation.
const MaxPrecision = 24

// depthTruncation returns the number of low-order bits dropped from samples
// of the given depth.
func depthTruncation(depth int) int {
	return max(0, depth-MaxPrecision)
}

// chargeFunc returns the charging callback used by the internal packages
// for a budget, or nil for no accounting.
func chargeFunc(b *membudget.Budget) func(int) error {
	if b == nil {
		return nil
	}
	return b.Charge
}
