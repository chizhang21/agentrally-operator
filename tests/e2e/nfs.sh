#!/usr/bin/env bash
# Sourced only after run.sh creates its disposable Kind and private kubeconfig.
install_nfs_fixture() {
  local platform=$2 evidence=$3 archive="$1/nfs-chart.tgz"
  curl -fsSL --retry 3 "https://github.com/kubernetes-sigs/nfs-ganesha-server-and-external-provisioner/releases/download/nfs-server-provisioner-$NFS_CHART_VERSION/nfs-server-provisioner-$NFS_CHART_VERSION.tgz" -o "$archive"
  echo "$NFS_CHART_SHA256  $archive" | sha256sum --check
  docker pull --platform="$platform" "$NFS_IMAGE@$NFS_IMAGE_DIGEST"
  docker tag "$NFS_IMAGE@$NFS_IMAGE_DIGEST" "$NFS_IMAGE"
  load_image "$NFS_IMAGE"
  printf 'nfs_chart=%s sha256=%s\nnfs_image=%s@%s\n' "$NFS_CHART_VERSION" "$NFS_CHART_SHA256" "$NFS_IMAGE" "$NFS_IMAGE_DIGEST" >> "$evidence"
  # This fresh test server has no pre-existing clients or locks to recover.
  helm install nfs "$archive" -n agentrally-test-storage --create-namespace \
    --set-string image.digest="$NFS_IMAGE_DIGEST" \
    --set extraArgs.grace-period=0 \
    --set storageClass.name=agentrally-test-nfs \
    --set 'storageClass.mountOptions={vers=4.1,retrans=2,timeo=30}' \
    --set persistence.enabled=true --set persistence.storageClass=standard \
    --wait --timeout=240s
}
