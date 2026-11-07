package cloudflare_test

// Shared fixture values used across cloudflare package tests.
const (
	testUUID1               = "00000000-0000-4000-8000-000000000001"
	testUUID2               = "00000000-0000-4000-8000-000000000002"
	testNetwork1            = "172.16.0.0/24"
	testNetwork2            = "172.16.1.0/24"
	testRecordTypeCNAME     = "CNAME"
	testRecordTypeTXT       = "TXT"
	testLeaseRecordName     = "_cfzt-lease.deadbeef.example.com"
	testDecisionAllow       = "allow"
	testAppsZoneID          = "apps"
	testConfigSrcCloudflare = "cloudflare"
	testTunnelName          = "homelab"
	testAppName             = "jellyfin-cfzt"
	testAdminDomain         = "jellyfin.example.com/admin"
	testHostname            = "jellyfin.example.com"
	testManagedTag          = "managed-by=cfzt-operator"
	testTunnelID            = "tunnel-1"
	testVnetID              = "vnet-1"
	testZoneID              = "zone-1"
)
