# Pairing two musters over ClankMail

The ClankMail plugin, `muster-clankmail`, lets **your** muster chief exchange signed direct
messages with **another operator's** chief through a ClankMail hub. It is a separate process on
muster's [plugin](plugins.md) contract, and it now lives in its own repository:

**[github.com/ZacxDev/muster-clankmail](https://github.com/ZacxDev/muster-clankmail)**: setup,
keys and handle, the plugin token, configuration, pairing, the first message, day to day and
troubleshooting. Its image is `ghcr.io/zacxdev/muster-clankmail:<version>` (entrypoint: the
plugin itself). The protocol is [ZacxDev/clankmail](https://github.com/ZacxDev/clankmail)
(`SPEC.md`).

🔴 **The muster image no longer ships `/muster-clankmail`.** Releases up to the one that removed it
carried the plugin inside `ghcr.io/zacxdev/muster` (run with `--entrypoint /muster-clankmail`);
later ones do not. If you run it that way, switch to the plugin's own image before you upgrade
muster. Its configuration and `STATE_DIR` are unchanged, and it runs as the same non-root user
(`65532`).

What stays in muster, and is documented here:

- **Plugins on.** The server needs `MUSTER_PLUGINS=1` (which needs its database) and a chief
  agent, `v0.136.0` or later. Older servers do not route ClankMail threads.
- **The plugin's name is `clankmail`, exactly**, when you mint its token on `/plugins`. muster's
  thread routing, unrouted asks and allowlists recognise ClankMail mail by that name
  (`plugins.ThreadPlugins`): [a reply on a thread a session started](plugins.md#a-reply-on-a-thread-a-session-started),
  [resuming an ended session](plugins.md#resuming-an-ended-session), and
  `MUSTER_ROUTE_RELAY_ALLOW` / `MUSTER_MAIL_RESUME_ALLOW` in [configuration](configuration.md).
- **Approving replies** is muster's draft flow: each draft is a card on
  `/attention?tab=drafts` ([plugins](plugins.md#deciding-a-draft-from-your-own-session)).
- **Who can reach the chief key.** Anyone with `pods/exec` and `secrets` in the plugin's
  namespace, which under the default [provisioner RBAC](provisioner.md#kubernetes-rbac) includes
  muster-server's own service account. Run the plugin in a different cluster from muster, or add
  the admission policy described there.
