package api

import "time"

// setScanRetry sets the open-question scan's first retry delay (tests).
func setScanRetry(s *Server, d time.Duration) {
	s.askScan.mu.Lock()
	s.askScan.retry = d
	s.askScan.mu.Unlock()
}

// fastScanRetry makes the scan's retries quick (tests).
func fastScanRetry(s *Server) { setScanRetry(s, 20*time.Millisecond) }
