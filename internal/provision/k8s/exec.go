package k8s

import (
	"context"
	"fmt"
	"io"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/client-go/kubernetes/scheme"
	"k8s.io/client-go/tools/remotecommand"

	"github.com/ZacxDev/muster/internal/provision"
)

// Exec implements provision.Execer.
//
// 🔴 IT IS UNCOVERED BY THIS PACKAGE'S TESTS, AND SAYING SO IS PART OF THE
// IMPLEMENTATION. The exec subresource is a SPDY/WebSocket upgrade against a
// live apiserver; client-go's fake clientset does not serve it, so no test here
// exercises the stream. What IS tested is the decision around it: that
// Capabilities.Exec is false without a RESTConfig, that provision.Exec refuses
// in that case, and that this method returns a named error rather than
// dereferencing a nil config.
//
// ⚠ A RELATED TRAP, RECORDED BECAUSE IT COST A WHOLE INVESTIGATION IN THE
// PROJECT THIS CAME FROM: `kubectl auth can-i get pods/log` answered YES for a
// service account whose real request then got 403. Do not verify exec or log
// access with an access review — make the call with the real token.
func (d *Driver) Exec(ctx context.Context, ref provision.Ref, cmd []string, stdin io.Reader, stdout, stderr io.Writer) error {
	if d.cfg.RESTConfig == nil {
		return fmt.Errorf("%w: this driver was built without a RESTConfig, so it cannot exec "+
			"(Capabilities.Exec is false and provision.Exec refuses before reaching here)", provision.ErrUnsupported)
	}
	if len(cmd) == 0 {
		return fmt.Errorf("%w: exec needs a command", provision.ErrInvalidSpec)
	}
	ns, pod, err := d.podFor(ctx, ref)
	if err != nil {
		return err
	}
	if pod == "" {
		return fmt.Errorf("%w: %q has no running pod to exec into", provision.ErrNotFound, ref.Name)
	}

	req := d.cfg.Client.CoreV1().RESTClient().
		Post().
		Resource("pods").
		Name(pod).
		Namespace(ns).
		SubResource("exec").
		VersionedParams(&corev1.PodExecOptions{
			// Named, for the same reason the log reads are. An unnamed
			// container silently means "the first one".
			Container: containerName,
			Command:   cmd,
			Stdin:     stdin != nil,
			Stdout:    stdout != nil,
			Stderr:    stderr != nil,
		}, scheme.ParameterCodec)

	exec, err := remotecommand.NewSPDYExecutor(d.cfg.RESTConfig, "POST", req.URL())
	if err != nil {
		return fmt.Errorf("exec into %s: %w", ref.Name, err)
	}
	return exec.StreamWithContext(ctx, remotecommand.StreamOptions{
		Stdin:  stdin,
		Stdout: stdout,
		Stderr: stderr,
	})
}
