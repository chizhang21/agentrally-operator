#!/usr/bin/env bash
# Sourced by e2e.sh after it creates a disposable cluster and private kubeconfig.
# Install upstream test components, without running downloaded shell scripts.

fetch_fixture() {
  local url=$1 checksum=$2 destination=$3
  curl -fsSL --retry 3 "$url" -o "$destination"
  echo "$checksum  $destination" | sha256sum --check
}

install_snapshot_fixtures() {
  local directory=$1
  fetch_fixture https://codeload.github.com/kubernetes-csi/external-snapshotter/tar.gz/refs/tags/v8.2.0 \
    ca0ffde826c2e67b358aa99ef224841b6f891f10dfcfa092926cb089ef3d65a4 "$directory/snapshotter.tar.gz"
  fetch_fixture https://codeload.github.com/kubernetes-csi/csi-driver-host-path/tar.gz/refs/tags/v1.16.1 \
    ac3f633111a8ca700d9d0023853a6b7a31e8f1f2797718091dc8285382c2e5fc "$directory/hostpath.tar.gz"
  tar -xzf "$directory/snapshotter.tar.gz" -C "$directory"
  tar -xzf "$directory/hostpath.tar.gz" -C "$directory"
  local snapshotter="$directory/external-snapshotter-8.2.0"
  local hostpath="$directory/csi-driver-host-path-1.16.1/deploy/kubernetes-1.30/hostpath"
  kubectl apply --server-side -f "$snapshotter/client/config/crd/snapshot.storage.k8s.io_volumesnapshotclasses.yaml" \
    -f "$snapshotter/client/config/crd/snapshot.storage.k8s.io_volumesnapshotcontents.yaml" \
    -f "$snapshotter/client/config/crd/snapshot.storage.k8s.io_volumesnapshots.yaml"
  kubectl wait --for=condition=Established --timeout=90s \
    crd/volumesnapshots.snapshot.storage.k8s.io crd/volumesnapshotcontents.snapshot.storage.k8s.io \
    crd/volumesnapshotclasses.snapshot.storage.k8s.io
  kubectl apply -f "$snapshotter/deploy/kubernetes/snapshot-controller/rbac-snapshot-controller.yaml" \
    -f "$snapshotter/deploy/kubernetes/snapshot-controller/setup-snapshot-controller.yaml"
  # Sidecar permissions match the image tags in the hostpath fixture.
  fetch_fixture https://raw.githubusercontent.com/kubernetes-csi/external-attacher/v4.8.0/deploy/kubernetes/rbac.yaml \
    3a38b5198b0496927c0ce26c63e71d43e2263e2dbbb93a83acdec71d7c90a766 "$directory/attacher-rbac.yaml"
  fetch_fixture https://raw.githubusercontent.com/kubernetes-csi/external-provisioner/v5.2.0/deploy/kubernetes/rbac.yaml \
    703cf589dbc799e8495b7a5ed3855d4804876055fae7933a84656d41e47f7507 "$directory/provisioner-rbac.yaml"
  fetch_fixture https://raw.githubusercontent.com/kubernetes-csi/external-resizer/v1.13.1/deploy/kubernetes/rbac.yaml \
    bc69d9d92e6dc477b3a71f09fef46f04f094bc3663a54098ab826b8c5b06abe7 "$directory/resizer-rbac.yaml"
  fetch_fixture https://raw.githubusercontent.com/kubernetes-csi/external-health-monitor/v0.14.0/deploy/kubernetes/external-health-monitor-controller/rbac.yaml \
    3205599eda9a252e2737acabe22591afd8da373c1aab5275ce54223adfe2d127 "$directory/health-rbac.yaml"
  kubectl apply -f "$directory/attacher-rbac.yaml" -f "$directory/provisioner-rbac.yaml" \
    -f "$directory/resizer-rbac.yaml" -f "$directory/health-rbac.yaml" \
    -f "$snapshotter/deploy/kubernetes/csi-snapshotter/rbac-csi-snapshotter.yaml"
  # Exclude the upstream socat testing manifest, which exposes the CSI socket.
  kubectl apply -f "$hostpath/csi-hostpath-driverinfo.yaml" -f "$hostpath/csi-hostpath-plugin.yaml" \
    -f "$hostpath/csi-hostpath-snapshotclass.yaml"
  kubectl -n kube-system rollout status deployment/snapshot-controller --timeout=240s
  kubectl rollout status statefulset/csi-hostpathplugin --timeout=360s
}
