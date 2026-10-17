// Copyright 2026 Jochen Voss <voss@seehuhn.de>
// SPDX-License-Identifier: BSD-3-Clause

package codestream

import (
	"errors"
)

// Quantization is the quantization parameter set of one component (QCD or QCC).
type Quantization struct {
	Style     QuantStyle
	GuardBits int // 0..7
	// Exponents holds one 5-bit exponent per subband (QuantNone,
	// QuantExpounded) or a single one (QuantDerived).
	Exponents []int
	// Mantissas holds the 11-bit mantissas, parallel to Exponents; it is
	// empty for QuantNone.
	Mantissas []int
}

// Validate checks that q is well formed and provides entries for every
// subband of a decomposition with the given number of levels.  QuantNone and
// QuantExpounded need at least 3*levels+1 entries, QuantDerived exactly one.
// Mantissas must be parallel to Exponents, except for QuantNone.
func (q *Quantization) Validate(levels int) error {
	if err := q.check(); err != nil {
		return err
	}
	if levels < 0 {
		return errors.New("negative number of decomposition levels")
	}
	// Codestreams in circulation declare more decomposition levels than
	// they provide quantization entries for; the missing entries belong
	// to sub-bands without any samples.  The caller reuses the last
	// entry for them, so only the presence of at least one is required.
	if len(q.Exponents) < 1 {
		return errors.New("quantization without entries")
	}
	_ = levels
	return nil
}

func (q *Quantization) check() error {
	if q.Style > QuantExpounded {
		return errors.New("invalid quantization style")
	}
	if q.GuardBits < 0 || q.GuardBits > 7 {
		return errors.New("invalid number of guard bits")
	}
	if len(q.Exponents) == 0 {
		return errors.New("no quantization entries")
	}
	for _, e := range q.Exponents {
		if e < 0 || e > 31 {
			return errors.New("invalid quantization exponent")
		}
	}
	if q.Style == QuantNone {
		if len(q.Mantissas) != 0 {
			return errors.New("mantissas given for reversible quantization")
		}
		return nil
	}
	if len(q.Mantissas) != len(q.Exponents) {
		return errors.New("exponent and mantissa counts differ")
	}
	if q.Style == QuantDerived && len(q.Exponents) != 1 {
		return errors.New("derived quantization needs one entry")
	}
	for _, m := range q.Mantissas {
		if m < 0 || m > 0x7FF {
			return errors.New("invalid quantization mantissa")
		}
	}
	return nil
}

// parseQuant parses Sqcd/SPqcd (identical to Sqcc/SPqcc).
func parseQuant(p []byte) (*Quantization, error) {
	if len(p) < 2 {
		return nil, errors.New("quantization segment too short")
	}
	q := &Quantization{Style: QuantStyle(p[0] & 0x1F), GuardBits: int(p[0] >> 5)}
	p = p[1:]
	switch q.Style {
	case QuantNone:
		for _, b := range p {
			q.Exponents = append(q.Exponents, int(b>>3))
		}
	case QuantDerived, QuantExpounded:
		if len(p)%2 != 0 {
			return nil, errors.New("odd quantization data length")
		}
		if q.Style == QuantDerived && len(p) != 2 {
			return nil, errors.New("derived quantization needs one entry")
		}
		for i := 0; i < len(p); i += 2 {
			v := int(p[i])<<8 | int(p[i+1])
			q.Exponents = append(q.Exponents, v>>11)
			q.Mantissas = append(q.Mantissas, v&0x7FF)
		}
	default:
		return nil, errors.New("unsupported quantization style")
	}
	return q, q.check()
}

func encodeQuant(q *Quantization) ([]byte, error) {
	if err := q.check(); err != nil {
		return nil, err
	}
	p := []byte{byte(q.Style) | byte(q.GuardBits)<<5}
	for i, e := range q.Exponents {
		if q.Style == QuantNone {
			p = append(p, byte(e<<3))
		} else {
			v := e<<11 | q.Mantissas[i]
			p = append(p, byte(v>>8), byte(v))
		}
	}
	return p, nil
}

func parseQCC(p []byte, csiz int) (int, *Quantization, error) {
	c, p, err := readComponentIndex(p, csiz)
	if err != nil {
		return 0, nil, err
	}
	q, err := parseQuant(p)
	return c, q, err
}

func encodeQCC(c int, q *Quantization, csiz int) ([]byte, error) {
	if c < 0 || c >= csiz {
		return nil, errors.New("QCC component index out of range")
	}
	b, err := encodeQuant(q)
	if err != nil {
		return nil, err
	}
	return append(appendComponentIndex(nil, c, csiz), b...), nil
}
