# calico-dscp-controller

- Check if dscp tag is set in iptables
```shell
 iptables -t mangle -L -n -v
```
Should see 
```
Chain POSTROUTING (policy ACCEPT 0 packets, 0 bytes)
pkts bytes target     prot opt in     out     source               destination         
0     0 DSCP       all  --  *      *       0.0.0.0/0            0.0.0.0/0            mark match 0xa DSCP set 0x0a
```
## Frequently used commmands:
- `kubeadm token create --print-join-command`