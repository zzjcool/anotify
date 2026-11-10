确认父 metadata 归属已单列。

固定五树完整测试已在上一轮实际执行一次（`-count=1`），退出 **0**，无需重复：
`/tmp/anotify-v4-delivery-source-name-gate3-test.log`

格式/范围、vet、完整 test 最终退出码为 **[0, 0, 0]**。首次红测及 ownership probe 日志均保留；13 个本 writer 授权产出与父 metadata 已分开记录。未再修改源码，等待 fresh G1 审查。

{"ok":true,"reason":"13 owned paths delivered; actual fixed five-tree full test already ran once with exit 0; full gates 0/0/0; parent metadata separated; fresh G1 review pending"}
