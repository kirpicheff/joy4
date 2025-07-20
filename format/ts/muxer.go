package ts

import (
	"fmt"
	"io"
	"log"
	"time"

	"github.com/datarhei/joy4/av"
	"github.com/datarhei/joy4/codec/aacparser"
	"github.com/datarhei/joy4/codec/h264parser"
	"github.com/datarhei/joy4/format/ts/tsio"
)

var CodecTypes = []av.CodecType{av.H264, av.AAC}

type Muxer struct {
	w                        io.Writer
	streams                  []*Stream
	PaddingToMakeCounterCont bool

	psidata []byte
	peshdr  []byte
	tshdr   []byte
	adtshdr []byte
	datav   [][]byte
	nalus   [][]byte

	tswpat, tswpmt *tsio.TSWriter

	// Добавляем счетчики для периодической отправки PAT/PMT
	patpmtCounter int
	lastPATPMT    time.Time

	// Отдельные счетчики для PAT и PMT
	patCounter uint
	pmtCounter uint

	// Счетчик для относительного времени
	startTime   time.Time
	packetCount uint64

	// Для отслеживания временных меток
	lastVideoTime time.Duration
	lastAudioTime time.Duration
}

func NewMuxer(w io.Writer) *Muxer {
	muxer := &Muxer{
		w:          w,
		psidata:    make([]byte, 188),
		peshdr:     make([]byte, tsio.MaxPESHeaderLength),
		tshdr:      make([]byte, tsio.MaxTSHeaderLength),
		adtshdr:    make([]byte, aacparser.ADTSHeaderLength),
		nalus:      make([][]byte, 16),
		datav:      make([][]byte, 16),
		tswpmt:     tsio.NewTSWriter(tsio.PMT_PID),
		tswpat:     tsio.NewTSWriter(tsio.PAT_PID),
		lastPATPMT: time.Now(),
	}

	// Возвращаем отдельные счетчики для PAT/PMT
	muxer.tswpat.SetGlobalCounter(&muxer.patCounter)
	muxer.tswpmt.SetGlobalCounter(&muxer.pmtCounter)

	return muxer
}

func (self *Muxer) newStream(codec av.CodecData) (err error) {
	ok := false
	for _, c := range CodecTypes {
		if codec.Type() == c {
			ok = true
			break
		}
	}
	if !ok {
		err = fmt.Errorf("ts: codec type=%s is not supported", codec.Type())
		return
	}

	pid := uint16(len(self.streams) + 0x100)
	stream := &Stream{
		muxer:     self,
		CodecData: codec,
		pid:       pid,
		tsw:       tsio.NewTSWriter(pid),
	}
	self.streams = append(self.streams, stream)
	return
}

func (self *Muxer) writePaddingTSPackets(tsw *tsio.TSWriter) (err error) {
	for tsw.ContinuityCounter&0xf != 0x0 {
		if err = tsw.WritePackets(self.w, self.datav[:0], 0, false, true); err != nil {
			return
		}
	}
	return
}

func (self *Muxer) WriteTrailer() (err error) {
	if self.PaddingToMakeCounterCont {
		for _, stream := range self.streams {
			if err = self.writePaddingTSPackets(stream.tsw); err != nil {
				return
			}
		}
	}
	return
}

func (self *Muxer) SetWriter(w io.Writer) {
	self.w = w
	return
}

func (self *Muxer) WritePATPMT() (err error) {
	pat := tsio.PAT{
		Entries: []tsio.PATEntry{
			{ProgramNumber: 1, ProgramMapPID: tsio.PMT_PID},
		},
	}
	patlen := pat.Marshal(self.psidata[tsio.PSIHeaderLength:])
	n := tsio.FillPSI(self.psidata, tsio.TableIdPAT, tsio.TableExtPAT, patlen)
	self.datav[0] = self.psidata[:n]
	if err = self.tswpat.WritePackets(self.w, self.datav[:1], 0, false, true); err != nil {
		return
	}

	var elemStreams []tsio.ElementaryStreamInfo
	for _, stream := range self.streams {
		switch stream.Type() {
		case av.AAC:
			elemStreams = append(elemStreams, tsio.ElementaryStreamInfo{
				StreamType:    tsio.ElementaryStreamTypeAdtsAAC,
				ElementaryPID: stream.pid,
			})
		case av.H264:
			elemStreams = append(elemStreams, tsio.ElementaryStreamInfo{
				StreamType:    tsio.ElementaryStreamTypeH264,
				ElementaryPID: stream.pid,
			})
		}
	}

	pmt := tsio.PMT{
		PCRPID:                0x100,
		ElementaryStreamInfos: elemStreams,
	}
	pmtlen := pmt.Len()
	if pmtlen+tsio.PSIHeaderLength > len(self.psidata) {
		err = fmt.Errorf("ts: pmt too large")
		return
	}
	pmt.Marshal(self.psidata[tsio.PSIHeaderLength:])
	n = tsio.FillPSI(self.psidata, tsio.TableIdPMT, tsio.TableExtPMT, pmtlen)
	self.datav[0] = self.psidata[:n]
	if err = self.tswpmt.WritePackets(self.w, self.datav[:1], 0, false, true); err != nil {
		return
	}

	self.lastPATPMT = time.Now()
	return
}

func (self *Muxer) WriteHeader(streams []av.CodecData) (err error) {
	self.streams = []*Stream{}
	for _, stream := range streams {
		if err = self.newStream(stream); err != nil {
			return
		}
	}

	if err = self.WritePATPMT(); err != nil {
		return
	}
	return
}

func (self *Muxer) WritePacket(pkt av.Packet) (err error) {
	// Инициализируем startTime при первом пакете
	if self.startTime.IsZero() {
		self.startTime = time.Now()
	}

	// ВОЗВРАЩАЕМ периодическую отправку PAT/PMT
	if time.Since(self.lastPATPMT) > 1*time.Second {
		if err = self.WritePATPMT(); err != nil {
			return
		}
	}

	stream := self.streams[pkt.Idx]

	// Логирование для диагностики временных меток
	if stream.Type() == av.H264 {
		// Проверяем промежуток между видео кадрами
		if self.lastVideoTime > 0 {
			timeDiff := pkt.Time - self.lastVideoTime
			if timeDiff > time.Second {
				log.Printf("WARNING: Large video time gap: %v between frames", timeDiff)
			}
		}
		self.lastVideoTime = pkt.Time

		log.Printf("VIDEO packet: time=%v, isKeyFrame=%v, compositionTime=%v, packetCount=%d",
			pkt.Time, pkt.IsKeyFrame, pkt.CompositionTime, self.packetCount)
	} else if stream.Type() == av.AAC {
		// Проверяем промежуток между аудио пакетами
		if self.lastAudioTime > 0 {
			timeDiff := pkt.Time - self.lastAudioTime
			if timeDiff > time.Second {
				log.Printf("WARNING: Large audio time gap: %v between packets", timeDiff)
			}
		}
		self.lastAudioTime = pkt.Time

		log.Printf("AUDIO packet: time=%v, packetCount=%d", pkt.Time, self.packetCount)
	}

	// Используем оригинальные временные метки без сложной конвертации
	originalTime := pkt.Time

	switch stream.Type() {
	case av.AAC:
		codec := stream.CodecData.(aacparser.CodecData)

		n := tsio.FillPESHeader(self.peshdr, tsio.StreamIdAAC, len(self.adtshdr)+len(pkt.Data), pkt.Time, 0)
		self.datav[0] = self.peshdr[:n]
		aacparser.FillADTSHeader(self.adtshdr, codec.Config, 1024, len(pkt.Data))
		self.datav[1] = self.adtshdr
		self.datav[2] = pkt.Data

		// ВОЗВРАЩАЕМ PCR для аудио пакетов с простым временем
		pcrTime := time.Duration(0)
		if self.packetCount%100 == 0 { // Каждые 100 пакетов
			pcrTime = time.Duration(self.packetCount) * time.Millisecond // Простое время
		}
		if err = stream.tsw.WritePackets(self.w, self.datav[:3], pcrTime, true, false); err != nil {
			return
		}

	case av.H264:
		codec := stream.CodecData.(h264parser.CodecData)

		nalus := self.nalus[:0]
		if pkt.IsKeyFrame {
			nalus = append(nalus, codec.SPS())
			nalus = append(nalus, codec.PPS())
		}
		pktnalus, _ := h264parser.SplitNALUs(pkt.Data)
		for _, nalu := range pktnalus {
			nalus = append(nalus, nalu)
		}

		datav := self.datav[:1]
		for i, nalu := range nalus {
			if i == 0 {
				datav = append(datav, h264parser.AUDBytes)
			} else {
				datav = append(datav, h264parser.StartCodeBytes)
			}
			datav = append(datav, nalu)
		}

		n := tsio.FillPESHeader(self.peshdr, tsio.StreamIdH264, -1, pkt.Time+pkt.CompositionTime, pkt.Time)
		datav[0] = self.peshdr[:n]

		// ВОЗВРАЩАЕМ PCR для видео с простым временем
		pcrTime := time.Duration(0)
		if pkt.IsKeyFrame && self.packetCount%100 == 0 {
			pcrTime = time.Duration(self.packetCount) * time.Millisecond // Простое время
		}
		if err = stream.tsw.WritePackets(self.w, datav, pcrTime, pkt.IsKeyFrame, false); err != nil {
			return
		}
	}

	// Увеличиваем счетчик пакетов
	self.packetCount++

	// Восстанавливаем оригинальное время для корректности
	pkt.Time = originalTime
	return
}
