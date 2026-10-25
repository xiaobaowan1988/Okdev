#!/bin/bash
# 无 systemd 环境下手动启动 kubelet:等 kubeadm 写出配置后再启动
while [ ! -f /var/lib/kubelet/config.yaml ] || [ ! -f /var/lib/kubelet/kubeadm-flags.env ]; do sleep 1; done
. /var/lib/kubelet/kubeadm-flags.env
exec kubelet --bootstrap-kubeconfig=/etc/kubernetes/bootstrap-kubelet.conf \
  --kubeconfig=/etc/kubernetes/kubelet.conf --config=/var/lib/kubelet/config.yaml \
  $KUBELET_KUBEADM_ARGS > /var/log/kubelet.log 2>&1
