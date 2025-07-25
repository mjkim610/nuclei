package smb

import (
	"context"
	"fmt"
	"net"
	"time"

	"github.com/praetorian-inc/fingerprintx/pkg/plugins"
	"github.com/praetorian-inc/fingerprintx/pkg/plugins/services/smb"
	"github.com/projectdiscovery/nuclei/v3/pkg/protocols/common/protocolstate"
	zgrabsmb "github.com/zmap/zgrab2/lib/smb/smb"
)

// ==== private helper functions/methods ====

// collectSMBv2Metadata collects metadata for SMBv2 services.
// @memo
func collectSMBv2Metadata(executionId string, host string, port int, timeout time.Duration) (*plugins.ServiceSMB, error) {
	if timeout == 0 {
		timeout = 5 * time.Second
	}
	dialer := protocolstate.GetDialersWithId(executionId)
	if dialer == nil {
		return nil, fmt.Errorf("dialers not initialized for %s", executionId)
	}

	conn, err := dialer.Fastdialer.Dial(context.TODO(), "tcp", net.JoinHostPort(host, fmt.Sprintf("%d", port)))
	if err != nil {
		return nil, err
	}
	defer func() {
		_ = conn.Close()
	}()

	metadata, err := smb.DetectSMBv2(conn, timeout)
	if err != nil {
		return nil, err
	}
	return metadata, nil
}

// getSMBInfo
func getSMBInfo(conn net.Conn, setupSession, v1 bool) (*zgrabsmb.SMBLog, error) {
	_ = conn.SetDeadline(time.Now().Add(10 * time.Second))
	defer func() {
		_ = conn.SetDeadline(time.Time{})
	}()

	result, err := zgrabsmb.GetSMBLog(conn, setupSession, v1, false)
	if err != nil {
		return nil, err
	}
	return result, nil
}
