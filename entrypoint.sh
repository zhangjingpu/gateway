#!/bin/bash

start_etcd() {
    ./etcd &
}

start_admin() {
    ./admin --registry-addr etcd://127.0.0.1:2379 --addr=:8080 &
}

start_proxy() {
    ./proxy --cfg ./config_etcd.json --log-level=$GATEWAY_LOG_LEVEL
}

start_etcd
sleep 3
start_admin
sleep 1
start_proxy

