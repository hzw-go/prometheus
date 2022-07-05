// Copyright 2017 The Prometheus Authors
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
// http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

// The code in this file was largely written by Damian Gryski as part of
// https://github.com/dgryski/go-tsz and published under the license below.
// It was modified to accommodate reading from byte slices without modifying
// the underlying bytes, which would panic when reading from mmap'd
// read-only byte slices.

// Copyright (c) 2015,2016 Damian Gryski <damian@gryski.com>
// All rights reserved.

// Redistribution and use in source and binary forms, with or without
// modification, are permitted provided that the following conditions are met:

// * Redistributions of source code must retain the above copyright notice,
// this list of conditions and the following disclaimer.
//
// * Redistributions in binary form must reproduce the above copyright notice,
// this list of conditions and the following disclaimer in the documentation
// and/or other materials provided with the distribution.
//
// THIS SOFTWARE IS PROVIDED BY THE COPYRIGHT HOLDERS AND CONTRIBUTORS "AS IS" AND
// ANY EXPRESS OR IMPLIED WARRANTIES, INCLUDING, BUT NOT LIMITED TO, THE IMPLIED
// WARRANTIES OF MERCHANTABILITY AND FITNESS FOR A PARTICULAR PURPOSE ARE
// DISCLAIMED. IN NO EVENT SHALL THE COPYRIGHT HOLDER OR CONTRIBUTORS BE LIABLE
// FOR ANY DIRECT, INDIRECT, INCIDENTAL, SPECIAL, EXEMPLARY, OR CONSEQUENTIAL
// DAMAGES (INCLUDING, BUT NOT LIMITED TO, PROCUREMENT OF SUBSTITUTE GOODS OR
// SERVICES; LOSS OF USE, DATA, OR PROFITS; OR BUSINESS INTERRUPTION) HOWEVER
// CAUSED AND ON ANY THEORY OF LIABILITY, WHETHER IN CONTRACT, STRICT LIABILITY,
// OR TORT (INCLUDING NEGLIGENCE OR OTHERWISE) ARISING IN ANY WAY OUT OF THE USE
// OF THIS SOFTWARE, EVEN IF ADVISED OF THE POSSIBILITY OF SUCH DAMAGE.

package chunkenc

import (
	"encoding/binary"
	"math"
	"math/bits"
)

const (
	chunkCompactCapacityThreshold = 32
)

// XORChunk holds XOR encoded sample data.
// XORChunk 实现Chunk接口
type XORChunk struct {
	// 存储时序点的数据
	b bstream
}

// NewXORChunk returns a new chunk with XOR encoding of the given size.
func NewXORChunk() *XORChunk {
	// 前两个byte存储时序点的个数
	b := make([]byte, 2, 128)
	return &XORChunk{b: bstream{stream: b, count: 0}}
}

// Encoding returns the encoding type.
func (c *XORChunk) Encoding() Encoding {
	return EncXOR
}

// Bytes returns the underlying byte slice of the chunk.
func (c *XORChunk) Bytes() []byte {
	return c.b.bytes()
}

// NumSamples returns the number of samples in the chunk.
func (c *XORChunk) NumSamples() int {
	// 前两个byte存储时序点的个数
	return int(binary.BigEndian.Uint16(c.Bytes()))
}

func (c *XORChunk) Compact() {
	if l := len(c.b.stream); cap(c.b.stream) > l+chunkCompactCapacityThreshold {
		buf := make([]byte, l)
		copy(buf, c.b.stream)
		c.b.stream = buf
	}
}

// Appender implements the Chunk interface.
// Appender 创建写入器，用于向该XORChunk追加时序点
func (c *XORChunk) Appender() (Appender, error) {
	it := c.iterator(nil)

	// To get an appender we must know the state it would have if we had
	// appended all existing data from scratch.
	// We iterate through the end and populate via the iterator's state.
	// 迭代到最后一个时序点
	for it.Next() {
	}
	if err := it.Err(); err != nil {
		return nil, err
	}

	a := &xorAppender{
		b:        &c.b,
		t:        it.t,
		v:        it.val,
		tDelta:   it.tDelta,
		leading:  it.leading,
		trailing: it.trailing,
	}
	// 空的XORChunk，初始化leading
	if binary.BigEndian.Uint16(a.b.bytes()) == 0 {
		a.leading = 0xff
	}
	return a, nil
}

// 创建迭代器，迭代XORChunk的时序点
func (c *XORChunk) iterator(it Iterator) *xorIterator {
	// Should iterators guarantee to act on a copy of the data so it doesn't lock append?
	// When using striped locks to guard access to chunks, probably yes.
	// Could only copy data if the chunk is not completed yet.
	if xorIter, ok := it.(*xorIterator); ok {
		xorIter.Reset(c.b.bytes())
		return xorIter
	}
	return &xorIterator{
		// The first 2 bytes contain chunk headers.
		// We skip that for actual samples.
		br:       newBReader(c.b.bytes()[2:]),
		numTotal: binary.BigEndian.Uint16(c.b.bytes()),
		t:        math.MinInt64,
	}
}

// Iterator implements the Chunk interface.
func (c *XORChunk) Iterator(it Iterator) Iterator {
	return c.iterator(it)
}

// Chunk文件写入接口实现
type xorAppender struct {
	// 存储时序点的数据
	b *bstream

	// 上次写入时序点的timestamp
	t      int64
	// 上次写入时序点的value
	v      float64
	// 当前点与前一个点的timestamp差值
	tDelta uint64

	// 当前XOR运算结果中前置0的个数
	leading  uint8
	// 当前XOR运算结果中后置0的个数
	trailing uint8
}

// Append 向XORChunk写入时序点：按照delta-of-delta方式压缩timestamp，按照XOR方式压缩value
func (a *xorAppender) Append(t int64, v float64) {
	var tDelta uint64
	num := binary.BigEndian.Uint16(a.b.bytes())

	if num == 0 {
		// 第一个时序点，需要写入完整的t、v
		buf := make([]byte, binary.MaxVarintLen64)
		for _, b := range buf[:binary.PutVarint(buf, t)] {
			a.b.writeByte(b)
		}
		a.b.writeBits(math.Float64bits(v), 64)

	} else if num == 1 {
		// 第二个时序点，记录delta，即t2-t1
		tDelta = uint64(t - a.t)

		buf := make([]byte, binary.MaxVarintLen64)
		for _, b := range buf[:binary.PutUvarint(buf, tDelta)] {
			a.b.writeByte(b)
		}

		a.writeVDelta(v)

	} else {
		// 第三个及以后的时序点
		tDelta = uint64(t - a.t)
		// 计算delta-of-delta
		dod := int64(tDelta - a.tDelta)

		// Gorilla has a max resolution of seconds, Prometheus milliseconds.
		// Thus we use higher value range steps with larger bit size.
		// delta-of-delta为0，则第一位写入0，否则写入1，同时对delta-of-delta进行变长编码
		switch {
		case dod == 0:
			a.b.writeBit(zero)
		case bitRange(dod, 14):
			a.b.writeBits(0b10, 2)
			a.b.writeBits(uint64(dod), 14)
		case bitRange(dod, 17):
			a.b.writeBits(0b110, 3)
			a.b.writeBits(uint64(dod), 17)
		case bitRange(dod, 20):
			a.b.writeBits(0b1110, 4)
			a.b.writeBits(uint64(dod), 20)
		default:
			a.b.writeBits(0b1111, 4)
			a.b.writeBits(uint64(dod), 64)
		}

		a.writeVDelta(v)
	}

	// 更新appender状态，为下一次append作准备
	a.t = t
	a.v = v
	binary.BigEndian.PutUint16(a.b.bytes(), num+1)
	a.tDelta = tDelta
}

// bitRange returns whether the given integer can be represented by nbits.
// See docs/bstream.md.
func bitRange(x int64, nbits uint8) bool {
	return -((1<<(nbits-1))-1) <= x && x <= 1<<(nbits-1)
}

// 向XORChunk写入value
func (a *xorAppender) writeVDelta(v float64) {
	// 计算当前v与上一个v的XOR值
	vDelta := math.Float64bits(v) ^ math.Float64bits(a.v)

	if vDelta == 0 {
		a.b.writeBit(zero)
		return
	}
	a.b.writeBit(one)

	// 计算XOR值的leading、trailing
	leading := uint8(bits.LeadingZeros64(vDelta))
	trailing := uint8(bits.TrailingZeros64(vDelta))

	// Clamp number of leading zeros to avoid overflow when encoding.
	if leading >= 32 {
		leading = 31
	}

	if a.leading != 0xff && leading >= a.leading && trailing >= a.trailing {
		// 此次计算得到的XOR结果中间的非0部分包含在前一个XOR运算结果中，则还是以上一次XOR结果所占字节数存储本次XOR结果，节约存储一个长度的空间
		a.b.writeBit(zero)
		a.b.writeBits(vDelta>>a.trailing, 64-int(a.leading)-int(a.trailing))
	} else {
		// 用5bit存储leading zeros，6bit存储有效XOR的长度
		a.leading, a.trailing = leading, trailing

		a.b.writeBit(one)
		a.b.writeBits(uint64(leading), 5)

		// Note that if leading == trailing == 0, then sigbits == 64.  But that value doesn't actually fit into the 6 bits we have.
		// Luckily, we never need to encode 0 significant bits, since that would put us in the other case (vdelta == 0).
		// So instead we write out a 0 and adjust it back to 64 on unpacking.
		sigbits := 64 - leading - trailing
		a.b.writeBits(uint64(sigbits), 6)
		a.b.writeBits(vDelta>>trailing, int(sigbits))
	}
}

// 读取XORChunk的时序点（从头开始重建）
type xorIterator struct {
	br       bstreamReader
	numTotal uint16
	numRead  uint16

	t   int64
	val float64

	leading  uint8
	trailing uint8

	tDelta uint64
	err    error
}

// Seek 跳到指定时间戳的时序点
func (it *xorIterator) Seek(t int64) bool {
	if it.err != nil {
		return false
	}

	for t > it.t || it.numRead == 0 {
		if !it.Next() {
			return false
		}
	}
	return true
}

func (it *xorIterator) At() (int64, float64) {
	return it.t, it.val
}

func (it *xorIterator) Err() error {
	return it.err
}

// Reset 重置迭代器的状态，从头开始迭代
func (it *xorIterator) Reset(b []byte) {
	// The first 2 bytes contain chunk headers.
	// We skip that for actual samples.
	it.br = newBReader(b[2:])
	it.numTotal = binary.BigEndian.Uint16(b)

	it.numRead = 0
	it.t = 0
	it.val = 0
	it.leading = 0
	it.trailing = 0
	it.tDelta = 0
	it.err = nil
}

// 读取下一个时序点
func (it *xorIterator) Next() bool {
	if it.err != nil || it.numRead == it.numTotal {
		return false
	}

	if it.numRead == 0 {
		// 第一个时序点，直接读取t、v
		t, err := binary.ReadVarint(&it.br)
		if err != nil {
			it.err = err
			return false
		}
		v, err := it.br.readBits(64)
		if err != nil {
			it.err = err
			return false
		}
		it.t = t
		it.val = math.Float64frombits(v)

		it.numRead++
		return true
	}
	if it.numRead == 1 {
		// 第二个时序点，读取delta，通过delta重建t
		tDelta, err := binary.ReadUvarint(&it.br)
		if err != nil {
			it.err = err
			return false
		}
		it.tDelta = tDelta
		it.t = it.t + int64(it.tDelta)

		return it.readValue()
	}

	var d byte
	// read delta-of-delta
	// 读取dod的控制位
	for i := 0; i < 4; i++ {
		d <<= 1
		bit, err := it.br.readBitFast()
		if err != nil {
			bit, err = it.br.readBit()
		}
		if err != nil {
			it.err = err
			return false
		}
		if bit == zero {
			break
		}
		d |= 1
	}
	//读取dod
	var sz uint8
	var dod int64
	switch d {
	case 0b0:
		// dod == 0
	case 0b10:
		sz = 14
	case 0b110:
		sz = 17
	case 0b1110:
		sz = 20
	case 0b1111:
		// Do not use fast because it's very unlikely it will succeed.
		bits, err := it.br.readBits(64)
		if err != nil {
			it.err = err
			return false
		}

		dod = int64(bits)
	}

	if sz != 0 {
		bits, err := it.br.readBitsFast(sz)
		if err != nil {
			bits, err = it.br.readBits(sz)
		}
		if err != nil {
			it.err = err
			return false
		}

		// Account for negative numbers, which come back as high unsigned numbers.
		// See docs/bstream.md.
		if bits > (1 << (sz - 1)) {
			bits -= 1 << sz
		}
		dod = int64(bits)
	}

	// 通过dod重建delta，通过delta重建t
	it.tDelta = uint64(int64(it.tDelta) + dod)
	it.t = it.t + int64(it.tDelta)

	return it.readValue()
}

// 根据XOR读取时序点的value
func (it *xorIterator) readValue() bool {
	// 读取第一个控制位
	bit, err := it.br.readBitFast()
	if err != nil {
		bit, err = it.br.readBit()
	}
	if err != nil {
		it.err = err
		return false
	}

	if bit == zero {
		// 与前一个val相同，无需任何操作
		// it.val = it.val
	} else {
		// 读取第二个控制位
		bit, err := it.br.readBitFast()
		if err != nil {
			bit, err = it.br.readBit()
		}
		if err != nil {
			it.err = err
			return false
		}
		if bit == zero {
			// reuse leading/trailing zero bits
			// it.leading, it.trailing = it.leading, it.trailing
			// 第二个控制位为0，代表可以复用前一个leading、trailing
		} else {
			// 读取leading、trailing
			bits, err := it.br.readBitsFast(5)
			if err != nil {
				bits, err = it.br.readBits(5)
			}
			if err != nil {
				it.err = err
				return false
			}
			it.leading = uint8(bits)

			bits, err = it.br.readBitsFast(6)
			if err != nil {
				bits, err = it.br.readBits(6)
			}
			if err != nil {
				it.err = err
				return false
			}
			mbits := uint8(bits)
			// 0 significant bits here means we overflowed and we actually need 64; see comment in encoder
			if mbits == 0 {
				mbits = 64
			}
			it.trailing = 64 - it.leading - mbits
		}

		// 根据leading、trailing读取XOR
		mbits := 64 - it.leading - it.trailing
		bits, err := it.br.readBitsFast(mbits)
		if err != nil {
			bits, err = it.br.readBits(mbits)
		}
		if err != nil {
			it.err = err
			return false
		}

		// 根据XOR和前一个值重建当前值（已知A、A XOR B，可以求B）
		vbits := math.Float64bits(it.val)
		vbits ^= bits << it.trailing
		it.val = math.Float64frombits(vbits)
	}

	it.numRead++
	return true
}
