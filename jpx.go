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
// component's native bit depth and signedness, with the inverse wavelet
// and component transforms applied but without any colour conversion or
// palette lookup.  The header reports everything the file says about the
// interpretation of the samples (colour specifications, channel
// definitions, palette), and helper functions perform the common
// conversions on request.
//
// The decoder is designed for untrusted input: it is written not to
// panic, bounds its memory use through a [Budget], and tolerates
// truncated or partially corrupt codestreams by returning the image
// decoded so far.
package jpx

import (
	"errors"
	"fmt"
)

// Budget accounts for working memory while decoding or encoding
// untrusted data.  Charge is called before every allocation whose size
// depends on the input, with the number of bytes about to be allocated;
// returning an error aborts the operation with that error.
type Budget interface {
	Charge(bytes int) error
}

// Component describes one image component as coded in the codestream.
type Component struct {
	// Depth is the number of bits per sample.  JPEG 2000 allows 1 to 38;
	// the decoder supports up to MaxDepth, the encoder up to
	// MaxEncodeDepth.
	Depth  int
	Signed bool // samples are two's complement when true, unsigned otherwise

	// DX and DY are the horizontal and vertical sub-sampling factors:
	// the component has one sample for every DX×DY points of the
	// reference grid.
	DX, DY int

	// Width and Height give the number of samples of the component at
	// full resolution.
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

// PaletteColumn describes the values of one palette column.
type PaletteColumn struct {
	Depth  int
	Signed bool
}

// Header describes a JPEG 2000 image without its sample data.
type Header struct {
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

// Plane holds the samples of one component.
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
	// samples are at the component's native depth and signedness.  With
	// Reduce > 0 the planes are correspondingly smaller.
	Planes []Plane
}

// Options controls decoding.
type Options struct {
	// Budget bounds the working memory, including the decoded planes;
	// only the copy of the input itself is exempt.  With a nil Budget
	// the decoder limits itself to DefaultBudgetBase plus
	// DefaultBudgetPerByte bytes for every byte of input, so that a small
	// hostile file cannot exhaust memory; images needing more must be
	// decoded with an explicit, larger budget.
	Budget Budget

	// Reduce discards this many resolution levels: the result has
	// roughly 1/2^Reduce of the width and height of the full image.
	Reduce int

	// Workers is the number of goroutines decoding tiles concurrently.
	// Zero or one decodes sequentially.  The Budget must be safe for
	// concurrent use when Workers is larger than one, and which of
	// several errors is reported may then vary between runs.
	Workers int
}

// MaxDepth is the largest sample precision the decoder supports.
const MaxDepth = 31

// ErrUnsupported is returned (possibly wrapped) when the input uses a
// feature outside the supported subset of JPEG 2000.
var ErrUnsupported = errors.New("jpx: unsupported feature")

func unsupported(what string) error {
	return fmt.Errorf("%w: %s", ErrUnsupported, what)
}

func chargeFunc(b Budget) func(int) error {
	if b == nil {
		return nil
	}
	return b.Charge
}
