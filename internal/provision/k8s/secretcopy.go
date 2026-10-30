package k8s

import (
	"context"
	"fmt"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/ZacxDev/muster/internal/provision"
)

// copyKeyHolderSecrets copies each Secret a key holder mounts
// (provision.SecretMount) from Config.SecretSourceNamespace — the namespace the
// operator keeps it in, muster's own — into the instance's namespace, before
// anything that could run is written. It does nothing when SecretSourceNamespace
// is empty (the operator places the Secret in the instance's namespace) or the
// spec has no key holder.
//
// 🔴 A MISSING SOURCE, OR ONE WITHOUT THE MOUNTED KEY, FAILS THE APPLY, NAMING
// THE SECRET. Started without it the pod would sit in ContainerCreating
// (FailedMount) forever, which reads as a slow worker.
//
// 🔴 ONLY THE DATA AND THE TYPE ARE COPIED, under muster's ownership labels: no
// annotation, no owner reference, nothing else of the source's. The copy is
// upserted through upsertOwned, so a co-named Secret muster did not make (one
// copied by hand) is refused, never overwritten; a source that changed since the
// last apply is written over the copy. The copy's bytes and the source's never
// reach a log line or an error: messages name objects only.
//
// ⚠ A CHANGED KEY REACHES A RUNNING POD AT ITS NEXT RESTART: the holder mounts it
// through a subPath, which the kubelet does not refresh. Stop -> Start does both
// (re-applies, so re-copies, and restarts).
//
// Removal: in the per-instance layout the namespace's deletion takes the copy.
// In the shared layout every worker of a pool mounts one copy under the source's
// name, so Destroy of one worker leaves it; and when the shared namespace IS the
// source's, nothing is copied (the Secret is already where the pod mounts it).
func (d *Driver) copyKeyHolderSecrets(ctx context.Context, spec provision.Spec, ns string) error {
	src := d.cfg.SecretSourceNamespace
	if redStub || src == "" || spec.KeyHolder == nil {
		return nil
	}
	c := d.cfg.Client.CoreV1()
	done := map[string]bool{}
	for _, m := range spec.KeyHolder.SecretMounts {
		from, err := c.Secrets(src).Get(ctx, m.Secret, metav1.GetOptions{})
		switch {
		case apierrors.IsNotFound(err):
			return fmt.Errorf("%w: the key holder mounts Secret %q, which does not exist in namespace %q (muster's "+
				"own, where it is copied from into the worker's namespace); create it there, then start the agent again. "+
				"Nothing that runs was written", provision.ErrInvalidSpec, m.Secret, src)
		case apierrors.IsForbidden(err):
			return fmt.Errorf("%w: the apiserver refused (403 Forbidden) muster's get of Secret %q in namespace %q: %v. "+
				"If the refusal is about RBAC, grant muster `get` on secrets in %q", provision.ErrUnsupported,
				m.Secret, src, apierrors.ReasonForError(err), src)
		case err != nil:
			return blind("get secret "+m.Secret+" in namespace "+src, err)
		}
		if _, ok := from.Data[m.Key]; !ok {
			return fmt.Errorf("%w: Secret %q in namespace %q has no key %q, which the key holder mounts; nothing that "+
				"runs was written", provision.ErrInvalidSpec, m.Secret, src, m.Key)
		}
		if ns == src || done[m.Secret] {
			continue
		}
		done[m.Secret] = true
		data := make(map[string][]byte, len(from.Data))
		for k, v := range from.Data {
			data[k] = append([]byte(nil), v...)
		}
		cp := &corev1.Secret{
			ObjectMeta: metav1.ObjectMeta{Name: m.Secret, Namespace: ns, Labels: copyLabels(managedLabels)},
			Type:       from.Type,
			Data:       data,
		}
		name := m.Secret
		if err := d.upsertOwned("secret", name, ns,
			func() (map[string]string, error) {
				return labelsOf(c.Secrets(ns).Get(ctx, name, metav1.GetOptions{}))
			},
			func() error {
				_, e := c.Secrets(ns).Create(ctx, cp, metav1.CreateOptions{})
				return e
			},
			func() error {
				_, e := c.Secrets(ns).Update(ctx, cp, metav1.UpdateOptions{})
				return e
			}); err != nil {
			if apierrors.IsForbidden(err) && !apierrors.HasStatusCause(err, corev1.NamespaceTerminatingCause) {
				return fmt.Errorf("%w: the apiserver refused (403 Forbidden) muster's copy of Secret %q into namespace %q: %v",
					provision.ErrUnsupported, name, ns, apierrors.ReasonForError(err))
			}
			return applyFailed("copy secret "+name+" from namespace "+src, err)
		}
	}
	return nil
}
