// Package dvb implements native Linux DVB (Digital Video Broadcasting)
// support: frontend enumeration, DVB API v5 tuning via FE_SET_PROPERTY,
// demux PID filtering, DVR TS capture and PSI/SI table parsing
// (PAT/PMT/SDT/NIT/EIT).
//
// This is new in the Go port — upstream C Movian has no DVB code and
// delegated TV to Tvheadend via HTSP. Here the goal is integrated
// playback without external servers.
package dvb

// DeliverySystem — enum fe_delivery_system (linux/dvb/frontend.h).
type DeliverySystem uint32

const (
	SysUndefined  DeliverySystem = 0
	SysDVBCAnnexA DeliverySystem = 1
	SysDVBCAnnexB DeliverySystem = 2
	SysDVBT       DeliverySystem = 3
	SysDSS        DeliverySystem = 4
	SysDVBS       DeliverySystem = 5
	SysDVBS2      DeliverySystem = 6
	SysDVBH       DeliverySystem = 7
	SysISDBT      DeliverySystem = 8
	SysISDBS      DeliverySystem = 9
	SysISDBC      DeliverySystem = 10
	SysATSC       DeliverySystem = 11
	SysATSCMH     DeliverySystem = 12
	SysDTMB       DeliverySystem = 13
	SysCMMB       DeliverySystem = 14
	SysDAB        DeliverySystem = 15
	SysDVBT2      DeliverySystem = 16
	SysTurbo      DeliverySystem = 17
	SysDVBCAnnexC DeliverySystem = 18
	SysDVBC2      DeliverySystem = 19
)

// IsTerrestrial reports whether the delivery system is terrestrial
// (DVB-T / DVB-T2) — used to pick the antenna frequency plan.
func (s DeliverySystem) IsTerrestrial() bool {
	return s == SysDVBT || s == SysDVBT2
}

// IsSatellite reports whether the delivery system is satellite
// (DVB-S / DVB-S2).
func (s DeliverySystem) IsSatellite() bool {
	return s == SysDVBS || s == SysDVBS2
}

// IsCable reports whether the delivery system is cable (any DVB-C annex).
func (s DeliverySystem) IsCable() bool {
	return s == SysDVBCAnnexA || s == SysDVBCAnnexB || s == SysDVBCAnnexC || s == SysDVBC2
}

func (s DeliverySystem) String() string {
	switch s {
	case SysDVBT:
		return "DVB-T"
	case SysDVBT2:
		return "DVB-T2"
	case SysDVBS:
		return "DVB-S"
	case SysDVBS2:
		return "DVB-S2"
	case SysDVBCAnnexA:
		return "DVB-C(A)"
	case SysDVBCAnnexB:
		return "DVB-C(B)"
	case SysDVBCAnnexC:
		return "DVB-C(C)"
	case SysDVBC2:
		return "DVB-C2"
	case SysATSC:
		return "ATSC"
	case SysISDBT:
		return "ISDB-T"
	default:
		return "unknown"
	}
}

// FrontendStatus — fe_status_t bits (linux/dvb/frontend.h).
type FrontendStatus uint32

const (
	StatNone       FrontendStatus = 0
	StatHasSignal  FrontendStatus = 0x01 // FE_HAS_SIGNAL
	StatHasCarrier FrontendStatus = 0x02 // FE_HAS_CARRIER
	StatHasViterbi FrontendStatus = 0x04 // FE_HAS_VITERBI
	StatHasSync    FrontendStatus = 0x08 // FE_HAS_SYNC
	StatHasLock    FrontendStatus = 0x10 // FE_HAS_LOCK
	StatTimedOut   FrontendStatus = 0x20 // FE_TIMEDOUT
	StatReinit     FrontendStatus = 0x40 // FE_REINIT
)

// Locked reports whether the frontend has a full demod lock.
func (s FrontendStatus) Locked() bool { return s&StatHasLock != 0 }

// Modulation — enum fe_modulation (QPSK..QAM_AUTO).
type Modulation uint32

const (
	ModQPSK    Modulation = 0
	ModQAM16   Modulation = 1
	ModQAM32   Modulation = 2
	ModQAM64   Modulation = 3
	ModQAM128  Modulation = 4
	ModQAM256  Modulation = 5
	ModQAMAuto Modulation = 6
	ModVSB8    Modulation = 7
	ModVSB16   Modulation = 8
	ModPSK8    Modulation = 9
	ModAPSK16  Modulation = 10
	ModAPSK32  Modulation = 11
	ModDQPSK   Modulation = 12
	ModQAM4NR  Modulation = 13
)

// CodeRate — enum fe_code_rate (FEC_NONE..FEC_AUTO).
type CodeRate uint32

const (
	FECNone CodeRate = iota
	FEC12
	FEC23
	FEC34
	FEC45
	FEC56
	FEC67
	FEC78
	FEC89
	FEC910
	FECAuto
	FEC35
	FEC920
	FEC25
)

// GuardInterval — enum fe_guard_interval.
type GuardInterval uint32

const (
	Guard132 GuardInterval = iota
	Guard116
	Guard18
	Guard14
	GuardAuto
	Guard1128
	Guard19128
	Guard19256
	GuardPN420
	GuardPN595
	GuardPN945
)

// TransmissionMode — enum fe_transmit_mode.
type TransmissionMode uint32

const (
	Tm2K TransmissionMode = iota
	Tm8K
	TmAuto
	Tm4K
	Tm1K
	Tm16K
	Tm32K
	TmC1
	TmC3780
)

// Inversion — enum fe_spectral_inversion.
type Inversion uint32

const (
	InversionOff  Inversion = 0
	InversionOn   Inversion = 1
	InversionAuto Inversion = 2
)

// FrontendCaps — enum fe_caps bits.
type FrontendCaps uint32

// FrontendInfo — decoded struct dvb_frontend_info.
type FrontendInfo struct {
	Name            string
	FrequencyMin    uint32 // Hz (kHz in the struct, normalized by caller)
	FrequencyMax    uint32
	FrequencyStep   uint32
	SymbolRateMin   uint32
	SymbolRateMax   uint32
	DeliverySystems []DeliverySystem // from DTV_ENUM_DELSYS
}

// TuneParams — one transponder/channel tuning request, delivery-system
// specific fields folded into a single struct; only the fields relevant
// to DeliverySys are sent in FE_SET_PROPERTY.
type TuneParams struct {
	DeliverySys DeliverySystem
	Frequency   uint32 // Hz
	Bandwidth   uint32 // Hz (0 = auto for DVB-T, 8 MHz typical)
	SymbolRate  uint32 // S/C: symbols/sec
	Modulation  Modulation
	InnerFEC    CodeRate
	Inversion   Inversion
	Guard       GuardInterval
	TxMode      TransmissionMode
	PLPID       uint32 // DVB-T2 data PLP id (0xffffffff = unset)
}

// Adapter describes one detected /dev/dvb/adapterN with its frontends.
type Adapter struct {
	ID        int            // adapter number (0 from adapter0)
	Frontends []FrontendInfo // one per frontendN node
	Path      string         // /dev/dvb/adapterN
}

// Channel is one scanned broadcast service.
type Channel struct {
	Name    string // service_name_descriptor (SDT)
	LCN     int    // logical channel number (NIT), 0 if absent
	Adapter int    // adapter index the channel was scanned on
	// Identifiers needed to tune/filter:
	Transport *TuneParams // transponder carrying the service
	TSID      uint16      // transport_stream_id
	ONID      uint16      // original_network_id
	SID       uint16      // service_id
	PMTPID    uint16      // from PAT
	PCRPID    uint16      // from PMT
	VideoPID  uint16      // first video elementary stream, 0 = none
	AudioPIDs []uint16    // audio elementary streams
	SubPIDs   []uint16    // DVB subtitle PIDs
	FreeCA    bool        // scrambled (CA_descriptor present)
}
