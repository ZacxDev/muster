// The step-up form script (internal/ui/stepup_confirm.go StepUpFormScript),
// run as the browser runs it against a minimal fake document: a form marked
// data-step-up-form, submitted by a NAMED button on a stale sign-in, asks
// first and — once confirmed — submits with that button's name and value
// (form.submit() sends no submitter: an ask answered by "End it" would
// otherwise submit no answer), and the dialog says the form's own gate.
//
//   node --test e2e/stepup-form.test.mjs
import { test } from 'node:test';
import assert from 'node:assert/strict';
import fs from 'node:fs';
import path from 'node:path';
import vm from 'node:vm';
import { fileURLToPath } from 'node:url';

const here = path.dirname(fileURLToPath(import.meta.url));
const go = fs.readFileSync(path.join(here, '..', 'internal', 'ui', 'stepup_confirm.go'), 'utf8');
const m = /const StepUpFormScript = `<script>([\s\S]*?)<\/script>`/.exec(go);

function world({ fresh }) {
  const listeners = [];
  const why = { firstChild: { nodeValue: 'The page gate. ' } };
  const appended = [];
  let submitted = 0;
  let asked = null;
  const form = {
    attrs: { 'data-step-up-why': 'Ending one of your team sessions needs a sign-in.' },
    matches: (sel) => sel === 'form[data-step-up-form]',
    getAttribute(n) { return this.attrs[n] ?? null; },
    appendChild: (el) => appended.push(el),
    submit: () => { submitted++; },
  };
  const button = { name: 'option', value: 'End it', form };
  const sandbox = {
    window: { musterStepUp: { fresh: () => fresh, ask: (w, done) => { asked = done; } } },
    document: {
      addEventListener: (type, fn) => listeners.push({ type, fn }),
      getElementById: (id) => (id === 'term-confirm-why' ? why : null),
      createElement: () => ({}),
    },
  };
  vm.runInNewContext(m[1], sandbox);
  let prevented = false;
  const ev = { target: form, submitter: button, preventDefault: () => { prevented = true; } };
  for (const l of listeners) if (l.type === 'submit') l.fn(ev);
  return { ev, why, appended, prevented: () => prevented, submitted: () => submitted, confirm: () => asked && asked() };
}

test('the script is found in the Go source', () => {
  assert.ok(m, 'StepUpFormScript was not found in stepup_confirm.go');
});

test('a stale submit by a named button asks first, then submits WITH that button', () => {
  const w = world({ fresh: false });
  assert.equal(w.prevented(), true, 'the stale submit was not held');
  assert.equal(w.submitted(), 0, 'it submitted before the confirm');
  assert.equal(w.why.firstChild.nodeValue, 'Ending one of your team sessions needs a sign-in. ', "the dialog does not say the form's own gate");
  w.confirm();
  assert.equal(w.submitted(), 1, 'the confirm did not submit the form');
  assert.equal(w.appended.length, 1, 'the tapped button was not carried');
  assert.deepEqual({ type: w.appended[0].type, name: w.appended[0].name, value: w.appended[0].value },
    { type: 'hidden', name: 'option', value: 'End it' });
});

test('a fresh submit is left alone (the control)', () => {
  const w = world({ fresh: true });
  assert.equal(w.prevented(), false);
  assert.equal(w.appended.length, 0);
});
