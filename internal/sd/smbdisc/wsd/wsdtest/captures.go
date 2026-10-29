package wsdtest

import (
	_ "embed"
	"strings"
)

// The sanitized WS-Discovery captures: what a Windows PC and a Samba
// host running wsdd put on the wire. They are the only copy; tests that
// parse them read them here.
var (
	// WindowsProbeMatch is a Windows PC's ProbeMatch for a Probe.
	//
	//go:embed testdata/windows-probematch.xml
	WindowsProbeMatch string
	// WindowsGetResponse is a Windows PC's answer to the Get.
	//
	//go:embed testdata/windows-getresponse.xml
	WindowsGetResponse string
	// WSDDProbeMatch is a wsdd host's ProbeMatch, which carries no XAddrs.
	//
	//go:embed testdata/wsdd-probematch.xml
	WSDDProbeMatch string
	// WSDDResolveMatch is a wsdd host's ResolveMatch, which carries its XAddr.
	//
	//go:embed testdata/wsdd-resolvematch.xml
	WSDDResolveMatch string
	// WSDDGetResponse is a wsdd host's answer to the Get.
	//
	//go:embed testdata/wsdd-getresponse.xml
	WSDDGetResponse string
)

// Values in the Windows captures that a fake host rewrites in each reply.
const (
	// CaptureRelatesTo is the RelatesTo of the Windows ProbeMatch.
	CaptureRelatesTo = "urn:uuid:67bb8bf2-d9ba-4257-8385-8b4e2642174f"
	// CaptureXAddr is the XAddr of the Windows ProbeMatch.
	CaptureXAddr = "http://192.0.2.10:5357/da5d9f02-f616-4bb9-a3df-6373ec52ec2b/"
	// CaptureEPR is the endpoint reference of the Windows captures.
	CaptureEPR = "urn:uuid:da5d9f02-f616-4bb9-a3df-6373ec52ec2b"
	// CaptureComputer is the pub:Computer element of the Windows GetResponse.
	CaptureComputer = "<pub:Computer>OFFICE-PC/Workgroup:WORKGROUP</pub:Computer>"
)

// The same values in the sanitized wsdd captures.
const (
	// WSDDProbeRelatesTo is the RelatesTo of the wsdd ProbeMatch.
	WSDDProbeRelatesTo = "urn:uuid:17f2fba5-c2aa-4ee9-80d4-de519a0ede35"
	// WSDDResolveRelatesTo is the RelatesTo of the wsdd ResolveMatch.
	WSDDResolveRelatesTo = "urn:uuid:2f4bc02b-3b72-4930-9661-14c072f60fe9"
	// WSDDXAddr is the XAddr of the wsdd ResolveMatch.
	WSDDXAddr = "http://192.0.2.20:5357/6f3f8dca-c31e-489b-906e-f4ce575c0067"
	// WSDDEPR is the endpoint reference of the wsdd captures.
	WSDDEPR = "urn:uuid:6f3f8dca-c31e-489b-906e-f4ce575c0067"
	// WSDDComputer is the pub:Computer element of the wsdd GetResponse.
	WSDDComputer = "<pub:Computer>FILESERVER/Workgroup:WORKGROUP</pub:Computer>"
)

// asWindows returns a wsdd capture with its values swapped for those of
// the Windows captures, which a fake host rewrites in each reply.
func asWindows(capture string) string {
	return strings.NewReplacer(
		WSDDProbeRelatesTo, CaptureRelatesTo,
		WSDDResolveRelatesTo, CaptureRelatesTo,
		WSDDXAddr, CaptureXAddr,
		WSDDEPR, CaptureEPR,
		WSDDComputer, CaptureComputer,
	).Replace(capture)
}
