# ConnectivityMonitorLogger

## Building

### Pull dependencies

```
go get ./...
```

### build a native binary

```
go build .
```

### build a linux/386 (EOS) binary

```
GOOS=linux GOARCH=386 go build .
```

## Switch configuration example 

```
daemon ConnectivityMonitorLogger
   exec /usr/sbin/ip netns exec ns-management /mnt/flash/ConnectivityMonitorLogger -addr localhost:5910 -username admin -interval 5 -rtt_threshold .1 -loss_threshold 1 -trigger_count 3 -rearm_count 2
   no shutdown
```

