package packet

import (
	"log"
	"sync/atomic"

	"github.com/CN-TU/go-flows/flows"
	"github.com/google/gopacket"
	"github.com/google/gopacket/layers"
)

type PacketBuffer interface {
	gopacket.Packet
	Forward() bool
	Timestamp() flows.DateTimeNanoseconds
	Key() flows.FlowKey
	Copy() PacketBuffer
	Hlen() int
	Proto() uint8
	Label() interface{}
	setInfo(flows.FlowKey, bool)
	Recycle()
	PacketNr() uint64
	decode() bool
}

type packetBuffer struct {
	inUse       int32
	owner       *multiPacketBuffer
	key         flows.FlowKey
	time        flows.DateTimeNanoseconds
	buffer      []byte
	first       gopacket.LayerType
	sll         layers.LinuxSLL
	eth         layers.Ethernet
	ip4         layers.IPv4
	ip6         layers.IPv6
	tcp         layers.TCP
	udp         layers.UDP
	icmpv4      ICMPv4Flow
	icmpv6      ICMPv6Flow
	link        gopacket.LinkLayer
	network     gopacket.NetworkLayer
	transport   gopacket.TransportLayer
	application gopacket.ApplicationLayer
	failure     gopacket.ErrorLayer
	ci          gopacket.PacketMetadata
	label       interface{}
	hlen        int
	refcnt      int
	packetnr    uint64
	proto       uint8
	forward     bool
	resize      bool
}

type SerializableLayerType interface {
	gopacket.SerializableLayer
	gopacket.Layer
}

var layerSerializeLengthBufferScratch []byte

type layerSerializeLengthBuffer struct {
	len int
}

func (w *layerSerializeLengthBuffer) Bytes() []byte {
	return layerSerializeLengthBufferScratch
}

func (w *layerSerializeLengthBuffer) PrependBytes(num int) ([]byte, error) {
	w.len += num
	return layerSerializeLengthBufferScratch, nil
}

func (w *layerSerializeLengthBuffer) AppendBytes(num int) ([]byte, error) {
	w.len += num
	return layerSerializeLengthBufferScratch, nil
}

func (w *layerSerializeLengthBuffer) Clear() error {
	w.len = 0
	return nil
}

func layerToLength(layer gopacket.SerializableLayer) int {
	if len(layerSerializeLengthBufferScratch) == 0 {
		layerSerializeLengthBufferScratch = make([]byte, 4096)
	}
	var b layerSerializeLengthBuffer
	layer.SerializeTo(&b, gopacket.SerializeOptions{})
	return b.len
}

func bufferFromLayers(when flows.DateTimeNanoseconds, layerList ...SerializableLayerType) (pb *packetBuffer) {
	pb = &packetBuffer{}
	pb.time = when
	for _, layer := range layerList {
		switch layer.LayerType() {
		case layers.LayerTypeEthernet:
			pb.first = layers.LayerTypeEthernet
			if pb.link != nil {
				log.Panic("Can only assign one Link Layer")
			}
			pb.link = layer.(gopacket.LinkLayer)
		case layers.LayerTypeLinuxSLL:
			pb.first = layers.LayerTypeLinuxSLL
			if pb.link != nil {
				log.Panic("Can only assign one Link Layer")
			}
			pb.link = layer.(gopacket.LinkLayer)
		case layers.LayerTypeIPv4:
			if pb.first != layers.LayerTypeEthernet {
				pb.first = layers.LayerTypeIPv4
			}
			if pb.network != nil {
				log.Panic("Can only assign one Network Layer")
			}
			pb.network = layer.(gopacket.NetworkLayer)
			pb.proto = uint8(pb.ip4.Protocol)
		case layers.LayerTypeIPv6:
			if pb.first != layers.LayerTypeEthernet {
				pb.first = layers.LayerTypeIPv6
			}
			if pb.network != nil {
				log.Panic("Can only assign one Network Layer")
			}
			pb.network = layer.(gopacket.NetworkLayer)
			pb.proto = uint8(pb.ip6.NextHeader)
		case layers.LayerTypeUDP:
			layer.(*layers.UDP).SetInternalPortsForTesting()
			if pb.first != layers.LayerTypeEthernet && pb.first != layers.LayerTypeIPv4 && pb.first != layers.LayerTypeIPv6 {
				pb.first = layers.LayerTypeUDP
			}
			if pb.transport != nil {
				log.Panic("Can only assign one Transport Layer")
			}
			pb.transport = layer.(gopacket.TransportLayer)
			if pb.proto == 0 {
				pb.proto = uint8(layers.IPProtocolUDP)
			}
		case layers.LayerTypeTCP:
			layer.(*layers.TCP).SetInternalPortsForTesting()
			if pb.first != layers.LayerTypeEthernet && pb.first != layers.LayerTypeIPv4 && pb.first != layers.LayerTypeIPv6 {
				pb.first = layers.LayerTypeTCP
			}
			if pb.transport != nil {
				log.Panic("Can only assign one Transport Layer")
			}
			pb.transport = layer.(gopacket.TransportLayer)
			if pb.proto == 0 {
				pb.proto = uint8(layers.IPProtocolTCP)
			}
		case layers.LayerTypeICMPv4:
			if pb.first != layers.LayerTypeEthernet && pb.first != layers.LayerTypeIPv4 && pb.first != layers.LayerTypeIPv6 {
				pb.first = layers.LayerTypeICMPv4
			}
			if pb.transport != nil {
				log.Panic("Can only assign one Transport Layer")
			}
			pb.transport = layer.(gopacket.TransportLayer)
			if pb.proto == 0 {
				pb.proto = uint8(layers.IPProtocolICMPv4)
			}
		case layers.LayerTypeICMPv6:
			if pb.first != layers.LayerTypeEthernet && pb.first != layers.LayerTypeIPv4 && pb.first != layers.LayerTypeIPv6 {
				pb.first = layers.LayerTypeICMPv6
			}
			if pb.transport != nil {
				log.Panic("Can only assign one Transport Layer")
			}
			pb.transport = layer.(gopacket.TransportLayer)
			if pb.proto == 0 {
				pb.proto = uint8(layers.IPProtocolICMPv6)
			}
		default:
			switch ip6e := layer.(type) {
			case *layers.IPv6Destination:
				pb.proto = uint8(ip6e.NextHeader)
			case *layers.IPv6HopByHop:
				pb.proto = uint8(ip6e.NextHeader)
			default:
				log.Panic("Protocol not supported")
			}
		}
		pb.hlen += layerToLength(layer)
	}
	if pb.network == nil {
		//add empty network layer (IPv4)
		if pb.first != layers.LayerTypeEthernet {
			pb.first = layers.LayerTypeIPv4
		}
		pb.ip4.SrcIP = []byte{0, 0, 0, 1}
		pb.ip4.DstIP = []byte{0, 0, 0, 2}
		pb.network = &pb.ip4
		pb.hlen += layerToLength(&pb.ip4)
	}
	if pb.transport == nil {
		pb.transport = &pb.udp
		if pb.proto == 0 {
			pb.proto = uint8(layers.IPProtocolUDP)
		}
		pb.hlen += layerToLength(&pb.ip4)
	}
	return
}

func (pb *packetBuffer) Hlen() int {
	return pb.hlen
}

func (pb *packetBuffer) Proto() uint8 {
	return pb.proto
}

func (pb *packetBuffer) PacketNr() uint64 {
	return pb.packetnr
}

func (pb *packetBuffer) Copy() PacketBuffer {
	pb.refcnt++
	return pb
}

func (pb *packetBuffer) assign(data []byte, ci gopacket.CaptureInfo, lt gopacket.LayerType, packetnr uint64, label interface{}) flows.DateTimeNanoseconds {
	pb.link = nil
	pb.network = nil
	pb.transport = nil
	pb.application = nil
	pb.failure = nil
	pb.tcp.Payload = nil
	pb.hlen = 0
	pb.refcnt = 1
	dlen := len(data)
	if pb.resize && cap(pb.buffer) < dlen {
		pb.buffer = make([]byte, dlen)
	} else if dlen < cap(pb.buffer) {
		pb.buffer = pb.buffer[0:dlen]
	} else {
		pb.buffer = pb.buffer[0:cap(pb.buffer)]
	}
	clen := copy(pb.buffer, data)
	pb.time = flows.DateTimeNanoseconds(ci.Timestamp.UnixNano())
	pb.ci.CaptureInfo = ci
	pb.ci.Truncated = ci.CaptureLength < ci.Length || clen < dlen
	pb.first = lt
	pb.label = label
	pb.packetnr = packetnr
	return pb.time
}

func (pb *packetBuffer) canRecycle() bool {
	pb.refcnt--
	if pb.refcnt > 0 {
		return false
	}
	return true
}

func (pb *packetBuffer) Recycle() {
	if !pb.canRecycle() {
		return
	}
	atomic.StoreInt32(&pb.inUse, 0)
	pb.owner.free(1)
}

func (pb *packetBuffer) Key() flows.FlowKey {
	return pb.key
}

func (pb *packetBuffer) Timestamp() flows.DateTimeNanoseconds {
	return pb.time
}

func (pb *packetBuffer) Forward() bool {
	return pb.forward
}

func (pb *packetBuffer) setInfo(key flows.FlowKey, forward bool) {
	pb.key = key
	pb.forward = forward
}

//DecodeFeedback
func (pb *packetBuffer) SetTruncated() { pb.ci.Truncated = true }

//gopacket.Packet
func (pb *packetBuffer) String() string { return "PacketBuffer" }
func (pb *packetBuffer) Dump() string   { return "" }
func (pb *packetBuffer) Layers() []gopacket.Layer {
	ret := make([]gopacket.Layer, 0, 3)
	if pb.link != nil {
		ret = append(ret, pb.link)
	}
	if pb.network != nil {
		ret = append(ret, pb.network)
	}
	if pb.transport != nil {
		ret = append(ret, pb.transport)
	}
	return ret
}
func (pb *packetBuffer) Layer(lt gopacket.LayerType) gopacket.Layer {
	if pb.link != nil && pb.link.LayerType() == lt {
		return pb.link
	}
	if pb.network != nil && pb.network.LayerType() == lt {
		return pb.network
	}
	if pb.transport != nil && pb.transport.LayerType() == lt {
		return pb.transport
	}
	return nil
}
func (pb *packetBuffer) LayerClass(lc gopacket.LayerClass) gopacket.Layer {
	if pb.link != nil && lc.Contains(pb.link.LayerType()) {
		return pb.link
	}
	if pb.network != nil && lc.Contains(pb.network.LayerType()) {
		return pb.network
	}
	if pb.transport != nil && lc.Contains(pb.transport.LayerType()) {
		return pb.transport
	}
	return nil
}
func (pb *packetBuffer) LinkLayer() gopacket.LinkLayer               { return pb.link }
func (pb *packetBuffer) NetworkLayer() gopacket.NetworkLayer         { return pb.network }
func (pb *packetBuffer) TransportLayer() gopacket.TransportLayer     { return pb.transport }
func (pb *packetBuffer) ApplicationLayer() gopacket.ApplicationLayer { return nil }
func (pb *packetBuffer) ErrorLayer() gopacket.ErrorLayer             { return nil }
func (pb *packetBuffer) Data() []byte                                { return pb.buffer }
func (pb *packetBuffer) Metadata() *gopacket.PacketMetadata          { return &pb.ci }
func (pb *packetBuffer) Label() interface{}                          { return pb.label }

//custom decoder for fun and speed. Borrowed from DecodingLayerParser
func (pb *packetBuffer) decode() (ret bool) {
	var ip6skipper layers.IPv6ExtensionSkipper
	defer func(r *bool) {
		if err := recover(); err != nil {
			if pb.tcp.Payload != nil {
				*r = true //fully decoded tcp packet except for options; should we count that as valid?
			} else {
				*r = false
			}
			//count decoding errors?
		}
	}(&ret)
	typ := pb.first
	var decoder gopacket.DecodingLayer
	data := pb.buffer
	for len(data) > 0 {
		switch typ {
		case layers.LayerTypeEthernet:
			decoder = &pb.eth
		case layers.LayerTypeLinuxSLL:
			decoder = &pb.sll
		case layers.LayerTypeIPv4:
			decoder = &pb.ip4
		case layers.LayerTypeIPv6:
			decoder = &pb.ip6
		case layers.LayerTypeUDP:
			decoder = &pb.udp
		case layers.LayerTypeTCP:
			decoder = &pb.tcp
		case layers.LayerTypeICMPv4:
			decoder = &pb.icmpv4
		case layers.LayerTypeICMPv6:
			decoder = &pb.icmpv6
		case layerTypeIPv46:
			version := data[0] >> 4
			switch version {
			case 4:
				decoder = &pb.ip4
				typ = layers.LayerTypeIPv4
			case 6:
				decoder = &pb.ip6
				typ = layers.LayerTypeIPv6
			default:
				return false
			}
		default:
			if layers.LayerClassIPv6Extension.Contains(typ) {
				decoder = &ip6skipper
			} else {
				return true
			}
		}
		if err := decoder.DecodeFromBytes(data, pb); err != nil {
			return false
		}
		switch typ {
		case layers.LayerTypeEthernet:
			pb.link = &pb.eth
			pb.hlen += len(pb.eth.Contents)
		case layers.LayerTypeLinuxSLL:
			pb.link = &pb.sll
		case layers.LayerTypeIPv4:
			pb.network = &pb.ip4
			pb.proto = uint8(pb.ip4.Protocol)
			pb.hlen += len(pb.ip4.Contents)
		case layers.LayerTypeIPv6:
			pb.network = &pb.ip6
			pb.proto = uint8(pb.ip6.NextHeader)
			if pb.proto == 0 { //fix hopbyhop
				pb.proto = uint8(pb.ip6.HopByHop.NextHeader)
			}
			pb.hlen += len(pb.ip6.Contents)
		case layers.LayerTypeUDP:
			pb.transport = &pb.udp
			pb.hlen += len(pb.udp.Contents)
			return true
		case layers.LayerTypeTCP:
			pb.transport = &pb.tcp
			pb.hlen += len(pb.tcp.Contents)
			return true
		case layers.LayerTypeICMPv4:
			pb.transport = &pb.icmpv4
			pb.hlen += len(pb.icmpv4.Contents)
			return true
		case layers.LayerTypeICMPv6:
			pb.transport = &pb.icmpv6
			pb.hlen += len(pb.icmpv6.Contents)
			return true
		default:
			if layers.LayerClassIPv6Extension.Contains(typ) {
				pb.proto = uint8(ip6skipper.NextHeader)
				pb.hlen += len(ip6skipper.Contents)
			}
		}
		typ = decoder.NextLayerType()
		data = decoder.LayerPayload()
	}
	return true
}
