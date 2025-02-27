// Copyright 2021 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package rsa

import (
	"math/big"
	"math/bits"
)

const (
	// _W is the number of bits we use for our limbs.
	_W = bits.UintSize - 1
	// _MASK selects _W bits from a full machine word.
	_MASK = (1 << _W) - 1
)

// choice represents a constant-time boolean. The value of choice is always
// either 1 or 0. We use an int instead of bool in order to make decisions in
// constant time by turning it into a mask.
type choice uint

func not(c choice) choice { return 1 ^ c }

const yes = choice(1)
const no = choice(0)

// ctSelect returns x if on == 1, and y if on == 0. The execution time of this
// function does not depend on its inputs. If on is any value besides 1 or 0,
// the result is undefined.
func ctSelect(on choice, x, y uint) uint {
	// When on == 1, mask is 0b111..., otherwise mask is 0b000...
	mask := -uint(on)
	// When mask is all zeros, we just have y, otherwise, y cancels with itself.
	return y ^ (mask & (y ^ x))
}

// ctEq returns 1 if x == y, and 0 otherwise. The execution time of this
// function does not depend on its inputs.
func ctEq(x, y uint) choice {
	// If x != y, then either x - y or y - x will generate a carry.
	_, c1 := bits.Sub(x, y, 0)
	_, c2 := bits.Sub(y, x, 0)
	return not(choice(c1 | c2))
}

// ctGeq returns 1 if x >= y, and 0 otherwise. The execution time of this
// function does not depend on its inputs.
func ctGeq(x, y uint) choice {
	// If x < y, then x - y generates a carry.
	_, carry := bits.Sub(x, y, 0)
	return not(choice(carry))
}

// nat represents an arbitrary natural number
//
// Each nat has an announced length, which is the number of limbs it has stored.
// Operations on this number are allowed to leak this length, but will not leak
// any information about the values contained in those limbs.
type nat struct {
	// limbs is a little-endian representation in base 2^W with
	// W = bits.UintSize - 1. The top bit is always unset between operations.
	//
	// The top bit is left unset to optimize Montgomery multiplication, in the
	// inner loop of exponentiation. Using fully saturated limbs would leave us
	// working with 129-bit numbers on 64-bit platforms, wasting a lot of space,
	// and thus time.
	limbs []uint
}

// preallocTarget is the size in bits of the numbers used to implement the most
// common and most performant RSA key size. It's also enough to cover some of
// the operations of key sizes up to 4096.
const preallocTarget = 2048
const preallocLimbs = (preallocTarget + _W) / _W

// newNat returns a new nat with a size of zero, just like new(nat), but with
// the preallocated capacity to hold a number of up to preallocTarget bits.
// newNat inlines, so the allocation can live on the stack.
func newNat() *nat {
	limbs := make([]uint, 0, preallocLimbs)
	return &nat{limbs}
}

// expand expands x to n limbs, leaving its value unchanged.
func (x *nat) expand(n int) *nat {
	for len(x.limbs) > n {
		if x.limbs[len(x.limbs)-1] != 0 {
			panic("rsa: internal error: shrinking nat")
		}
		x.limbs = x.limbs[:len(x.limbs)-1]
	}
	if cap(x.limbs) < n {
		newLimbs := make([]uint, n)
		copy(newLimbs, x.limbs)
		x.limbs = newLimbs
		return x
	}
	extraLimbs := x.limbs[len(x.limbs):n]
	for i := range extraLimbs {
		extraLimbs[i] = 0
	}
	x.limbs = x.limbs[:n]
	return x
}

// reset returns a zero nat of n limbs, reusing x's storage if n <= cap(x.limbs).
func (x *nat) reset(n int) *nat {
	if cap(x.limbs) < n {
		x.limbs = make([]uint, n)
		return x
	}
	for i := range x.limbs {
		x.limbs[i] = 0
	}
	x.limbs = x.limbs[:n]
	return x
}

// set assigns x = y, optionally resizing x to the appropriate size.
func (x *nat) set(y *nat) *nat {
	x.reset(len(y.limbs))
	copy(x.limbs, y.limbs)
	return x
}

// set assigns x = n, optionally resizing n to the appropriate size.
//
// The announced length of x is set based on the actual bit size of the input,
// ignoring leading zeroes.
func (x *nat) setBig(n *big.Int) *nat {
	bitSize := bigBitLen(n)
	requiredLimbs := (bitSize + _W - 1) / _W
	x.reset(requiredLimbs)

	outI := 0
	shift := 0
	limbs := n.Bits()
	for i := range limbs {
		xi := uint(limbs[i])
		x.limbs[outI] |= (xi << shift) & _MASK
		outI++
		if outI == requiredLimbs {
			return x
		}
		x.limbs[outI] = xi >> (_W - shift)
		shift++ // this assumes bits.UintSize - _W = 1
		if shift == _W {
			shift = 0
			outI++
		}
	}
	return x
}

// fillBytes sets bytes to x as a zero-extended big-endian byte slice.
//
// If bytes is not long enough to contain the number or at least len(x.limbs)-1
// limbs, or has zero length, fillBytes will panic.
func (x *nat) fillBytes(bytes []byte) []byte {
	if len(bytes) == 0 {
		panic("nat: fillBytes invoked with too small buffer")
	}
	for i := range bytes {
		bytes[i] = 0
	}
	shift := 0
	outI := len(bytes) - 1
	for i, limb := range x.limbs {
		remainingBits := _W
		for remainingBits >= 8 {
			bytes[outI] |= byte(limb) << shift
			consumed := 8 - shift
			limb >>= consumed
			remainingBits -= consumed
			shift = 0
			outI--
			if outI < 0 {
				if limb != 0 || i < len(x.limbs)-1 {
					panic("nat: fillBytes invoked with too small buffer")
				}
				return bytes
			}
		}
		bytes[outI] = byte(limb)
		shift = remainingBits
	}
	return bytes
}

// setBytes assigns x = b, where b is a slice of big-endian bytes, optionally
// resizing n to the appropriate size.
//
// The announced length of the output depends only on the length of b. Unlike
// big.Int, creating a nat will not remove leading zeros.
func (x *nat) setBytes(b []byte) *nat {
	bitSize := len(b) * 8
	requiredLimbs := (bitSize + _W - 1) / _W
	x.reset(requiredLimbs)

	outI := 0
	shift := 0
	for i := len(b) - 1; i >= 0; i-- {
		bi := b[i]
		x.limbs[outI] |= uint(bi) << shift
		shift += 8
		if shift >= _W {
			shift -= _W
			x.limbs[outI] &= _MASK
			outI++
			if shift > 0 {
				x.limbs[outI] = uint(bi) >> (8 - shift)
			}
		}
	}
	return x
}

// cmpEq returns 1 if x == y, and 0 otherwise.
//
// Both operands must have the same announced length.
func (x *nat) cmpEq(y *nat) choice {
	// Eliminate bounds checks in the loop.
	size := len(x.limbs)
	xLimbs := x.limbs[:size]
	yLimbs := y.limbs[:size]

	equal := yes
	for i := 0; i < size; i++ {
		equal &= ctEq(xLimbs[i], yLimbs[i])
	}
	return equal
}

// cmpGeq returns 1 if x >= y, and 0 otherwise.
//
// Both operands must have the same announced length.
func (x *nat) cmpGeq(y *nat) choice {
	// Eliminate bounds checks in the loop.
	size := len(x.limbs)
	xLimbs := x.limbs[:size]
	yLimbs := y.limbs[:size]

	var c uint
	for i := 0; i < size; i++ {
		c = (xLimbs[i] - yLimbs[i] - c) >> _W
	}
	// If there was a carry, then subtracting y underflowed, so
	// x is not greater than or equal to y.
	return not(choice(c))
}

// assign sets x <- y if on == 1, and does nothing otherwise.
//
// Both operands must have the same announced length.
func (x *nat) assign(on choice, y *nat) *nat {
	// Eliminate bounds checks in the loop.
	size := len(x.limbs)
	xLimbs := x.limbs[:size]
	yLimbs := y.limbs[:size]

	for i := 0; i < size; i++ {
		xLimbs[i] = ctSelect(on, yLimbs[i], xLimbs[i])
	}
	return x
}

// add computes x += y if on == 1, and does nothing otherwise. It returns the
// carry of the addition regardless of on.
//
// Both operands must have the same announced length.
func (x *nat) add(on choice, y *nat) (c uint) {
	// Eliminate bounds checks in the loop.
	size := len(x.limbs)
	xLimbs := x.limbs[:size]
	yLimbs := y.limbs[:size]

	for i := 0; i < size; i++ {
		res := xLimbs[i] + yLimbs[i] + c
		xLimbs[i] = ctSelect(on, res&_MASK, xLimbs[i])
		c = res >> _W
	}
	return
}

// sub computes x -= y if on == 1, and does nothing otherwise. It returns the
// borrow of the subtraction regardless of on.
//
// Both operands must have the same announced length.
func (x *nat) sub(on choice, y *nat) (c uint) {
	// Eliminate bounds checks in the loop.
	size := len(x.limbs)
	xLimbs := x.limbs[:size]
	yLimbs := y.limbs[:size]

	for i := 0; i < size; i++ {
		res := xLimbs[i] - yLimbs[i] - c
		xLimbs[i] = ctSelect(on, res&_MASK, xLimbs[i])
		c = res >> _W
	}
	return
}

// modulus is used for modular arithmetic, precomputing relevant constants.
//
// Moduli are assumed to be odd numbers. Moduli can also leak the exact
// number of bits needed to store their value, and are stored without padding.
//
// Their actual value is still kept secret.
type modulus struct {
	// The underlying natural number for this modulus.
	//
	// This will be stored without any padding, and shouldn't alias with any
	// other natural number being used.
	nat     *nat
	leading int  // number of leading zeros in the modulus
	m0inv   uint // -nat.limbs[0]⁻¹ mod _W
	RR      *nat // R*R for montgomeryRepresentation
}

// rr returns R*R with R = 2^(_W * n) and n = len(m.nat.limbs).
func rr(m *modulus) *nat {
	rr := newNat().expandFor(m)
	// R*R is 2^(2 * _W * n). We can safely get 2^(_W * (n - 1)) by setting the
	// most significant limb to 1. We then get to R*R by shifting left by _W
	// n + 1 times.
	n := len(rr.limbs)
	rr.limbs[n-1] = 1
	for i := n - 1; i < 2*n; i++ {
		rr.shiftIn(0, m) // x = x * 2^_W mod m
	}
	return rr
}

// minusInverseModW computes -x⁻¹ mod _W with x odd.
//
// This operation is used to precompute a constant involved in Montgomery
// multiplication.
func minusInverseModW(x uint) uint {
	// Every iteration of this loop doubles the least-significant bits of
	// correct inverse in y. The first three bits are already correct (1⁻¹ = 1,
	// 3⁻¹ = 3, 5⁻¹ = 5, and 7⁻¹ = 7 mod 8), so doubling five times is enough
	// for 61 bits (and wastes only one iteration for 31 bits).
	//
	// See https://crypto.stackexchange.com/a/47496.
	y := x
	for i := 0; i < 5; i++ {
		y = y * (2 - x*y)
	}
	return (1 << _W) - (y & _MASK)
}

// modulusFromNat creates a new modulus from a nat.
//
// The nat should be odd, nonzero, and the number of significant bits in the
// number should be leakable. The nat shouldn't be reused.
func modulusFromNat(nat *nat) *modulus {
	m := &modulus{}
	m.nat = nat
	size := len(m.nat.limbs)
	for m.nat.limbs[size-1] == 0 {
		size--
	}
	m.nat.limbs = m.nat.limbs[:size]
	m.leading = _W - bitLen(m.nat.limbs[size-1])
	m.m0inv = minusInverseModW(m.nat.limbs[0])
	m.RR = rr(m)
	return m
}

// bitLen is a version of bits.Len that only leaks the bit length of n, but not
// its value. bits.Len and bits.LeadingZeros use a lookup table for the
// low-order bits on some architectures.
func bitLen(n uint) int {
	var len int
	// We assume, here and elsewhere, that comparison to zero is constant time
	// with respect to different non-zero values.
	for n != 0 {
		len++
		n >>= 1
	}
	return len
}

// bigBitLen is a version of big.Int.BitLen that only leaks the bit length of x,
// but not its value. big.Int.BitLen uses bits.Len.
func bigBitLen(x *big.Int) int {
	xLimbs := x.Bits()
	fullLimbs := len(xLimbs) - 1
	topLimb := uint(xLimbs[len(xLimbs)-1])
	return fullLimbs*bits.UintSize + bitLen(topLimb)
}

// modulusSize returns the size of m in bytes.
func modulusSize(m *modulus) int {
	bits := len(m.nat.limbs)*_W - int(m.leading)
	return (bits + 7) / 8
}

// shiftIn calculates x = x << _W + y mod m.
//
// This assumes that x is already reduced mod m, and that y < 2^_W.
func (x *nat) shiftIn(y uint, m *modulus) *nat {
	d := newNat().resetFor(m)

	// Eliminate bounds checks in the loop.
	size := len(m.nat.limbs)
	xLimbs := x.limbs[:size]
	dLimbs := d.limbs[:size]
	mLimbs := m.nat.limbs[:size]

	// Each iteration of this loop computes x = 2x + b mod m, where b is a bit
	// from y. Effectively, it left-shifts x and adds y one bit at a time,
	// reducing it every time.
	//
	// To do the reduction, each iteration computes both 2x + b and 2x + b - m.
	// The next iteration (and finally the return line) will use either result
	// based on whether the subtraction underflowed.
	needSubtraction := no
	for i := _W - 1; i >= 0; i-- {
		carry := (y >> i) & 1
		var borrow uint
		for i := 0; i < size; i++ {
			l := ctSelect(needSubtraction, dLimbs[i], xLimbs[i])

			res := l<<1 + carry
			xLimbs[i] = res & _MASK
			carry = res >> _W

			res = xLimbs[i] - mLimbs[i] - borrow
			dLimbs[i] = res & _MASK
			borrow = res >> _W
		}
		// See modAdd for how carry (aka overflow), borrow (aka underflow), and
		// needSubtraction relate.
		needSubtraction = ctEq(carry, borrow)
	}
	return x.assign(needSubtraction, d)
}

// mod calculates out = x mod m.
//
// This works regardless how large the value of x is.
//
// The output will be resized to the size of m and overwritten.
func (out *nat) mod(x *nat, m *modulus) *nat {
	out.resetFor(m)
	// Working our way from the most significant to the least significant limb,
	// we can insert each limb at the least significant position, shifting all
	// previous limbs left by _W. This way each limb will get shifted by the
	// correct number of bits. We can insert at least N - 1 limbs without
	// overflowing m. After that, we need to reduce every time we shift.
	i := len(x.limbs) - 1
	// For the first N - 1 limbs we can skip the actual shifting and position
	// them at the shifted position, which starts at min(N - 2, i).
	start := len(m.nat.limbs) - 2
	if i < start {
		start = i
	}
	for j := start; j >= 0; j-- {
		out.limbs[j] = x.limbs[i]
		i--
	}
	// We shift in the remaining limbs, reducing modulo m each time.
	for i >= 0 {
		out.shiftIn(x.limbs[i], m)
		i--
	}
	return out
}

// expandFor ensures out has the right size to work with operations modulo m.
//
// This assumes that out has as many or fewer limbs than m, or that the extra
// limbs are all zero (which may happen when decoding a value that has leading
// zeroes in its bytes representation that spill over the limb threshold).
func (out *nat) expandFor(m *modulus) *nat {
	return out.expand(len(m.nat.limbs))
}

// resetFor ensures out has the right size to work with operations modulo m.
//
// out is zeroed and may start at any size.
func (out *nat) resetFor(m *modulus) *nat {
	return out.reset(len(m.nat.limbs))
}

// modSub computes x = x - y mod m.
//
// The length of both operands must be the same as the modulus. Both operands
// must already be reduced modulo m.
func (x *nat) modSub(y *nat, m *modulus) *nat {
	underflow := x.sub(yes, y)
	// If the subtraction underflowed, add m.
	x.add(choice(underflow), m.nat)
	return x
}

// modAdd computes x = x + y mod m.
//
// The length of both operands must be the same as the modulus. Both operands
// must already be reduced modulo m.
func (x *nat) modAdd(y *nat, m *modulus) *nat {
	overflow := x.add(yes, y)
	underflow := not(x.cmpGeq(m.nat)) // x < m

	// Three cases are possible:
	//
	//   - overflow = 0, underflow = 0
	//
	// In this case, addition fits in our limbs, but we can still subtract away
	// m without an underflow, so we need to perform the subtraction to reduce
	// our result.
	//
	//   - overflow = 0, underflow = 1
	//
	// The addition fits in our limbs, but we can't subtract m without
	// underflowing. The result is already reduced.
	//
	//   - overflow = 1, underflow = 1
	//
	// The addition does not fit in our limbs, and the subtraction's borrow
	// would cancel out with the addition's carry. We need to subtract m to
	// reduce our result.
	//
	// The overflow = 1, underflow = 0 case is not possible, because y is at
	// most m - 1, and if adding m - 1 overflows, then subtracting m must
	// necessarily underflow.
	needSubtraction := ctEq(overflow, uint(underflow))

	x.sub(needSubtraction, m.nat)
	return x
}

// montgomeryRepresentation calculates x = x * R mod m, with R = 2^(_W * n) and
// n = len(m.nat.limbs).
//
// Faster Montgomery multiplication replaces standard modular multiplication for
// numbers in this representation.
//
// This assumes that x is already reduced mod m.
func (x *nat) montgomeryRepresentation(m *modulus) *nat {
	// A Montgomery multiplication (which computes a * b / R) by R * R works out
	// to a multiplication by R, which takes the value out of the Montgomery domain.
	return x.montgomeryMul(newNat().set(x), m.RR, m)
}

// montgomeryReduction calculates x = x / R mod m, with R = 2^(_W * n) and
// n = len(m.nat.limbs).
//
// This assumes that x is already reduced mod m.
func (x *nat) montgomeryReduction(m *modulus) *nat {
	// By Montgomery multiplying with 1 not in Montgomery representation, we
	// convert out back from Montgomery representation, because it works out to
	// dividing by R.
	t0 := newNat().set(x)
	t1 := newNat().expandFor(m)
	t1.limbs[0] = 1
	return x.montgomeryMul(t0, t1, m)
}

// montgomeryMul calculates d = a * b / R mod m, with R = 2^(_W * n) and
// n = len(m.nat.limbs), using the Montgomery Multiplication technique.
//
// All inputs should be the same length, not aliasing d, and already
// reduced modulo m. d will be resized to the size of m and overwritten.
func (d *nat) montgomeryMul(a *nat, b *nat, m *modulus) *nat {
	// See https://bearssl.org/bigint.html#montgomery-reduction-and-multiplication
	// for a description of the algorithm.

	// Eliminate bounds checks in the loop.
	size := len(m.nat.limbs)
	aLimbs := a.limbs[:size]
	bLimbs := b.limbs[:size]
	dLimbs := d.resetFor(m).limbs[:size]
	mLimbs := m.nat.limbs[:size]

	var overflow uint
	for i := 0; i < size; i++ {
		f := ((dLimbs[0] + aLimbs[i]*bLimbs[0]) * m.m0inv) & _MASK
		carry := uint(0)
		for j := 0; j < size; j++ {
			// z = d[j] + a[i] * b[j] + f * m[j] + carry <= 2^(2W+1) - 2^(W+1) + 2^W
			hi, lo := bits.Mul(aLimbs[i], bLimbs[j])
			z_lo, c := bits.Add(dLimbs[j], lo, 0)
			z_hi, _ := bits.Add(0, hi, c)
			hi, lo = bits.Mul(f, mLimbs[j])
			z_lo, c = bits.Add(z_lo, lo, 0)
			z_hi, _ = bits.Add(z_hi, hi, c)
			z_lo, c = bits.Add(z_lo, carry, 0)
			z_hi, _ = bits.Add(z_hi, 0, c)
			if j > 0 {
				dLimbs[j-1] = z_lo & _MASK
			}
			carry = z_hi<<1 | z_lo>>_W // carry <= 2^(W+1) - 2
		}
		z := overflow + carry // z <= 2^(W+1) - 1
		dLimbs[size-1] = z & _MASK
		overflow = z >> _W // overflow <= 1
	}
	// See modAdd for how overflow, underflow, and needSubtraction relate.
	underflow := not(d.cmpGeq(m.nat)) // d < m
	needSubtraction := ctEq(overflow, uint(underflow))
	d.sub(needSubtraction, m.nat)

	return d
}

// modMul calculates x *= y mod m.
//
// x and y must already be reduced modulo m, they must share its announced
// length, and they may not alias.
func (x *nat) modMul(y *nat, m *modulus) *nat {
	// A Montgomery multiplication by a value out of the Montgomery domain
	// takes the result out of Montgomery representation.
	xR := newNat().set(x).montgomeryRepresentation(m) // xR = x * R mod m
	return x.montgomeryMul(xR, y, m)                  // x = xR * y / R mod m
}

// exp calculates out = x^e mod m.
//
// The exponent e is represented in big-endian order. The output will be resized
// to the size of m and overwritten. x must already be reduced modulo m.
func (out *nat) exp(x *nat, e []byte, m *modulus) *nat {
	// We use a 4 bit window. For our RSA workload, 4 bit windows are faster
	// than 2 bit windows, but use an extra 12 nats worth of scratch space.
	// Using bit sizes that don't divide 8 are more complex to implement.

	table := [(1 << 4) - 1]*nat{ // table[i] = x ^ (i+1)
		// newNat calls are unrolled so they are allocated on the stack.
		newNat(), newNat(), newNat(), newNat(), newNat(),
		newNat(), newNat(), newNat(), newNat(), newNat(),
		newNat(), newNat(), newNat(), newNat(), newNat(),
	}
	table[0].set(x).montgomeryRepresentation(m)
	for i := 1; i < len(table); i++ {
		table[i].montgomeryMul(table[i-1], table[0], m)
	}

	out.resetFor(m)
	out.limbs[0] = 1
	out.montgomeryRepresentation(m)
	t0 := newNat().expandFor(m)
	t1 := newNat().expandFor(m)
	for _, b := range e {
		for _, j := range []int{4, 0} {
			// Square four times.
			t1.montgomeryMul(out, out, m)
			out.montgomeryMul(t1, t1, m)
			t1.montgomeryMul(out, out, m)
			out.montgomeryMul(t1, t1, m)

			// Select x^k in constant time from the table.
			k := uint((b >> j) & 0b1111)
			for i := range table {
				t0.assign(ctEq(k, uint(i+1)), table[i])
			}

			// Multiply by x^k, discarding the result if k = 0.
			t1.montgomeryMul(out, t0, m)
			out.assign(not(ctEq(k, 0)), t1)
		}
	}

	return out.montgomeryReduction(m)
}
