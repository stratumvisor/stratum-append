# Customer overlay layout example

For content destined for two different in-container paths, assemble a working tree:

```text
overlay/
  stratum/
    html/templates/intel/customvm/customvm.yaml
    addons/qemu/customvm/sata.qcow2
```

Then:

```bash
stratum-container-append --input stratum-container.tar-ORIG \
  --output stratum-container.tar --source overlay --dest /
```

You can also create a portable layer tar:

```bash
tar -C overlay -cf customvm-layer.tar .
stratum-container-append --input stratum-container.tar-ORIG \
  --output stratum-container.tar --layer customvm-layer.tar
```

