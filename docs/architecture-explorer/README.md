# Interactive architecture explorer

Open [index.html](index.html) in a browser.
The page is a standalone HTML file with inline SVG, CSS and JavaScript.
It needs no build step, external assets or cluster access.

The eight views cover the system, resource relationships, allocation, lifecycle,
workspace ownership, capability discovery, access paths and implementation boundaries.
Click graph components for details and repository source paths.
The flow player supports cold and warm allocation, suspension/resumption,
execution replacement and deletion.
Definition descriptions include opt-in stateless preparation before the first Instance.
Publication Ready and capacity Prepared describe separate observations.

All interactions are explanatory simulations.
They never submit Kubernetes requests.
The workspace comparison distinguishes private PVCs, external PVCs and the opt-in
platform persistence preview.
Deletion recovery, retention countdown, automatic GC and exclusive handoff remain
unavailable in the current implementation.

Keep the resource descriptions, graph edges and flow steps aligned with
[the architecture](../architecture.md) and their linked source files when behavior changes.
The download button exports a standalone copy for offline use.
