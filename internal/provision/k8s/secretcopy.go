package k8s

import (
	"context"
	"fmt"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/ZacxDev/muster/internal/provision"
)

// copySecrets copies the operator's Secrets an instance needs from
// Config.SecretSourceNamespace — the namespace the operator keeps them in,
// muster's own — into the instance's namespace, before anything that could run
// is written. It is ONE copy rule for two users:
//
//   - each Secret a key holder mounts (provision.SecretMount): its mounted keys;
//   - the image pull Secret (provision.Spec.ImagePullSecret): its
//     `.dockerconfigjson` key only, and only from a source of type
//     `kubernetes.io/dockerconfigjson`.
//
// One Secret is never both (provision.Spec.Validate refuses it, and
// muster-server refuses such a configuration at boot), so each copy holds
// exactly its own keys. It does nothing when SecretSourceNamespace is empty (the operator places the Secrets
// in the instance's namespace) or the spec names none.
//
// 🔴 A MISSING SOURCE, OR ONE WITHOUT A NEEDED KEY, FAILS THE APPLY, NAMING THE
// SECRET — and so does a pull Secret of another type. Started without it the pod
// would sit in ContainerCreating (FailedMount) or ImagePullBackOff forever,
// which reads as a slow worker.
//
// 🔴 ONLY THE NEEDED KEYS AND THE TYPE ARE COPIED, under muster's ownership
// labels: no other key of the source (a public half, a second key the operator
// keeps beside it), no annotation, no owner reference. The copy is upserted
// through upsertOwned, so a co-named Secret muster did not make (one copied by
// hand) is refused, never overwritten; a source that changed since the last
// apply is written over the copy. The copy's bytes and the source's never reach
// a log line or an error: messages name objects only.
//
// 🔴 A SOURCE RECREATED WITH ANOTHER TYPE REPLACES THE COPY. A Secret's type is
// immutable, so the apiserver answers an Update changing it with 422, and every
// Start would fail until the namespace was recreated. Instead, on the update
// path — reached only for a copy upsertOwned has confirmed is muster's — a copy
// whose type differs is deleted (preconditioned on the UID just read) and
// created again.
//
// ⚠ A CHANGED KEY REACHES A RUNNING POD AT ITS NEXT RESTART: the holder mounts it
// through a subPath, which the kubelet does not refresh; a pull credential is
// read only when an image is pulled. Stop -> Start does both (re-applies, so
// re-copies, and restarts).
//
// Removal: in the per-instance layout the namespace's deletion takes the copy.
// In the shared layout every worker of a pool uses one copy under the source's
// name, so Destroy of one worker leaves it; and when the shared namespace IS the
// source's, nothing is copied (the Secret is already where the pod uses it).
func (d *Driver) copySecrets(ctx context.Context, spec provision.Spec, ns string) error {
	src := d.cfg.SecretSourceNamespace
	if src == "" || (spec.KeyHolder == nil && spec.ImagePullSecret == "") {
		return nil
	}
	c := d.cfg.Client.CoreV1()
	// Pass 1: every source exists and has its keys, before anything is
	// written; and which keys of each Secret are copied.
	sources := map[string]*corev1.Secret{}
	keys := map[string][]string{}
	var order []string
	// source reads one source Secret once; user is who needs it, for the
	// refusal ("the key holder mounts", "the instance's image is pulled with").
	source := func(name, user string) (*corev1.Secret, error) {
		if from, seen := sources[name]; seen {
			return from, nil
		}
		from, err := c.Secrets(src).Get(ctx, name, metav1.GetOptions{})
		switch {
		case apierrors.IsNotFound(err):
			return nil, fmt.Errorf("%w: %s Secret %q, which does not exist in namespace %q (muster's "+
				"own, where it is copied from into the worker's namespace); create it there, then start the agent again. "+
				"Nothing that runs was written", provision.ErrInvalidSpec, user, name, src)
		case apierrors.IsForbidden(err):
			return nil, fmt.Errorf("%w: the apiserver refused (403 Forbidden) muster's get of Secret %q in namespace %q: %v. "+
				"If the refusal is about RBAC, grant muster `get` on that one Secret in %q: a Role with resources: [secrets], "+
				"resourceNames: [%s], verbs: [get], bound to muster's service account", provision.ErrUnsupported,
				name, src, apierrors.ReasonForError(err), src, name)
		case err != nil:
			return nil, blind("get secret "+name+" in namespace "+src, err)
		}
		sources[name] = from
		order = append(order, name)
		return from, nil
	}
	if spec.KeyHolder != nil {
		for _, m := range spec.KeyHolder.SecretMounts {
			from, err := source(m.Secret, "the key holder mounts")
			if err != nil {
				return err
			}
			if _, ok := from.Data[m.Key]; !ok {
				return fmt.Errorf("%w: Secret %q in namespace %q has no key %q, which the key holder mounts; nothing that "+
					"runs was written", provision.ErrInvalidSpec, m.Secret, src, m.Key)
			}
			keys[m.Secret] = append(keys[m.Secret], m.Key)
		}
	}
	if name := spec.ImagePullSecret; name != "" {
		from, err := source(name, "the instance's image is pulled with")
		if err != nil {
			return err
		}
		if from.Type != corev1.SecretTypeDockerConfigJson {
			return fmt.Errorf("%w: Secret %q in namespace %q is of type %q; an image pull Secret must be %q "+
				"(holding key %q). Nothing that runs was written", provision.ErrInvalidSpec, name, src, from.Type,
				corev1.SecretTypeDockerConfigJson, corev1.DockerConfigJsonKey)
		}
		if _, ok := from.Data[corev1.DockerConfigJsonKey]; !ok {
			return fmt.Errorf("%w: Secret %q in namespace %q has no key %q, which the instance's image is pulled with; "+
				"nothing that runs was written", provision.ErrInvalidSpec, name, src, corev1.DockerConfigJsonKey)
		}
		// Never also a key holder's (Spec.Validate refuses the overlap), so
		// this copy holds exactly its own key.
		keys[name] = []string{corev1.DockerConfigJsonKey}
	}
	if ns == src {
		return nil
	}
	// Pass 2: one copy per Secret, holding only the keys pass 1 named.
	for _, name := range order {
		from := sources[name]
		data := make(map[string][]byte, len(keys[name]))
		for _, k := range keys[name] {
			data[k] = append([]byte(nil), from.Data[k]...)
		}
		labels := copyLabels(managedLabels)
		if name == spec.ImagePullSecret {
			// Marks it as THE pull copy, so sweepPullCopies can tell it from a
			// key holder's copy.
			labels[labelPullCopy] = "true"
		}
		cp := &corev1.Secret{
			ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: ns, Labels: labels},
			Type:       from.Type,
			Data:       data,
		}
		if err := d.upsertOwned("secret", name, ns,
			func() (map[string]string, error) {
				return labelsOf(c.Secrets(ns).Get(ctx, name, metav1.GetOptions{}))
			},
			func() error {
				_, e := c.Secrets(ns).Create(ctx, cp, metav1.CreateOptions{})
				return e
			},
			func() error {
				// Reached only after upsertOwned confirmed the copy is muster's.
				cur, e := c.Secrets(ns).Get(ctx, name, metav1.GetOptions{})
				if e != nil {
					return e
				}
				if cur.Type == cp.Type {
					_, e = c.Secrets(ns).Update(ctx, cp, metav1.UpdateOptions{})
					return e
				}
				uid := cur.UID
				if e := c.Secrets(ns).Delete(ctx, name, metav1.DeleteOptions{
					Preconditions: &metav1.Preconditions{UID: &uid}}); e != nil && !apierrors.IsNotFound(e) {
					return e
				}
				_, e = c.Secrets(ns).Create(ctx, cp, metav1.CreateOptions{})
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

// labelPullCopy marks a Secret copySecrets copied as an instance's image pull
// Secret (provision.Spec.ImagePullSecret): sweepPullCopies deletes only
// Secrets carrying it AND muster's ownership labels, never a key holder's copy
// or a Secret muster did not make.
const labelPullCopy = "muster.dev/image-pull-copy"

// previousPullSecrets is what the instance's live Deployment pulls with now:
// the names in its pod spec's imagePullSecrets. None when there is no
// Deployment or it cannot be read. (A Deployment muster did not make is
// refused by the write that follows, before any sweep; and the sweep deletes
// only muster's own pull copies whatever names this returns.)
func (d *Driver) previousPullSecrets(ctx context.Context, ns, name string) []string {
	dep, err := d.cfg.Client.AppsV1().Deployments(ns).Get(ctx, name, metav1.GetOptions{})
	if err != nil {
		return nil
	}
	var out []string
	for _, r := range dep.Spec.Template.Spec.ImagePullSecrets {
		out = append(out, r.Name)
	}
	return out
}

// sweepPullCopies deletes the stale image pull Secret copies of an instance
// once its Deployment is written: each of prev (previousPullSecrets, read
// before the write) that is not current — the variable renamed, unset, or the
// pool's own image dropped (current "") — and is muster's pull copy
// (ownership labels, labelPullCopy, type kubernetes.io/dockerconfigjson). The
// delete is preconditioned on the UID just read.
//
// ⚠ PER-INSTANCE NAMESPACES ONLY: in a shared namespace one copy serves every
// worker that pulls with it, so one worker's change must not delete it. A
// failure is logged, never the apply's: a stale copy holds a credential the
// operator still keeps in the source, and the next apply sweeps again.
func (d *Driver) sweepPullCopies(ctx context.Context, ns string, prev []string, current string) {
	if !d.cfg.NamespacePerInstance || ns == d.cfg.SecretSourceNamespace {
		return
	}
	c := d.cfg.Client.CoreV1()
	for _, name := range prev {
		if name == current {
			continue
		}
		cur, err := c.Secrets(ns).Get(ctx, name, metav1.GetOptions{})
		if apierrors.IsNotFound(err) {
			continue
		}
		if err != nil {
			d.log.Printf("k8s: the stale image pull Secret copy %s in namespace %s could not be read: %v", name, ns, apierrors.ReasonForError(err))
			continue
		}
		if !owned(cur.Labels) || cur.Labels[labelPullCopy] != "true" || cur.Type != corev1.SecretTypeDockerConfigJson {
			continue
		}
		uid := cur.UID
		if err := c.Secrets(ns).Delete(ctx, name, metav1.DeleteOptions{Preconditions: &metav1.Preconditions{UID: &uid}}); err != nil && !apierrors.IsNotFound(err) {
			d.log.Printf("k8s: the stale image pull Secret copy %s in namespace %s was not deleted: %v", name, ns, apierrors.ReasonForError(err))
		}
	}
}
