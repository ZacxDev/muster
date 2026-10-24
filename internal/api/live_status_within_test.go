package api

import (
	"bytes"
	"context"
	"log"
	"sync"
	"testing"
	"time"

	"github.com/ZacxDev/muster/internal/provision"
)

// answeringProv answers Instances at once with one instance.
type answeringProv struct{ stubProvisioner }

func (answeringProv) Instances(context.Context) ([]provision.Instance, error) {
	return []provision.Instance{{Ref: provision.Ref{Name: "a"}}}, nil
}

// TestABoundedInstanceReadThatAnswersIsNeverDroppedAsATimeout: a provisioner
// that answers at once, read with an hour's bound, is observed every time —
// never a nil index ("not observed") and never the "no answer within" line.
//
// The bug this guards: the read's goroutine cancelled ctx right after sending
// its answer, so a caller that reached the select late found both cases ready
// and dropped the answer at random. That made the stopped-agent reaper destroy
// nothing under CI load (TestTheReaperIsOffAtZero,
// TestAStoppedRowOverALiveInstanceIsNotReaped). Whether a given call loses
// depends on scheduling, so the read is repeated from many goroutines at once;
// on the old code this loses within the first few hundred calls.
func TestABoundedInstanceReadThatAnswersIsNeverDroppedAsATimeout(t *testing.T) {
	var buf lockedBuf
	s := &Server{logger: log.New(&buf, "", 0), ext: Extensions{Provisioner: answeringProv{}}}
	const workers, each = 16, 2000
	var wg sync.WaitGroup
	var mu sync.Mutex
	dropped := 0
	for w := 0; w < workers; w++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := 0; i < each; i++ {
				if idx := s.instanceIndexWithin(context.Background(), time.Hour); idx == nil || idx["a"] == nil {
					mu.Lock()
					dropped++
					mu.Unlock()
				}
			}
		}()
	}
	wg.Wait()
	if dropped != 0 {
		t.Fatalf("%d of %d answered reads came back as not observed; log:\n%s", dropped, workers*each, firstLines(buf.String(), 3))
	}
	if bytes.Contains([]byte(buf.String()), []byte("no answer within")) {
		t.Fatalf("an answered read was logged as a timeout:\n%s", firstLines(buf.String(), 3))
	}
}

func firstLines(s string, n int) string {
	lines := bytes.SplitN([]byte(s), []byte("\n"), n+1)
	if len(lines) > n {
		lines = lines[:n]
	}
	return string(bytes.Join(lines, []byte("\n")))
}
