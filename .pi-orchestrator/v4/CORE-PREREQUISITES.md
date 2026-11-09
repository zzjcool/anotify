# 核心精确前置裁决 · C2 Source / C1 predicate

## Source.metadata.displayName

已冻结可写字段：api-conventions.md ObjectMeta字段表、Source可写表、Source完整示例三处明确；source.md未枚举不表示排除。代码缺Source字段与DDL是实现缺口，不撤需求、不塞config。

父批准C2精准扩owner：schema.sql.sources.display_name、source.go结构/列/scan/创建更新/CAS、migrate必需布局Source字段检查及v4test往返/CAS。可选展示值按统一metadata规则，PATCH省略不改；展示更新不得改变name/kind/targets/payload/token/secret。G3只投影metadata，不另加扁平DTO或新资源。

仍开发期直建baseline，无历史迁移；已format4缺列也SchemaInvalid拒，不自动补列/覆盖旧数据。新库/reopen/字段往返/同版CAS并发/省略配置不变测试保持。

## C1 partial unique predicate

`emit_id IS (NOT NULL)`语义为IS(NULL)，不是`emit_id IS NOT NULL`。不能删除所有括号再比较文本，这会让无效partialunique通过。

只migrate与formattests精修：保留运算/分组位置，接受明确baseline规范形式及包住整个谓词的括号；也允许仅包住精确列标识 `emit_id` 的配对括号（如 `(([emit_id]) IS NOT NULL)`），这不改变列语义。标识可使用 SQLite 合法 identifier 引号，不能把字符串常量、表达式、函数、运算符或 `NOT NULL` 操作数括号当作列标识归一。其余未知变体 failclosed；不建立通用 SQL parser、不扩大支持历史 DDL。

父澄清：『只接受整体表达式括号』原意是禁止任意抹除运算子表达式的分组，并非要求拒绝语义相同的列标识括号。已修 `IS (NOT NULL)` 的实现若仅额外保留上述安全标识形式，无需第三次代码修改；必须保留有害谓词拒绝、合法标识正例和数据不变测试，独立复审按此精确规则判定。

真实反例先红：SQLite该错误predicate对非null重复不生效，带v4marker的这种布局必须SchemaInvalid；合法baselineIS NOT NULL/允许wholeexpr括号通过。保留原用户数据/结构，不改查验时的数据库。

## 顺序与验收

原C2 writer正确拒绝且无改动已retired，不盲重启。先C1-R2小修safe settle，再新C2 Source前置+CAS扩卡后的单writer；schema/migrate不并发写。旧194机械PASS为历史，freshC1gate未清；修后原阶段gate+精准独立review，完整C3/旧测试/G2-G4和最终全Go/contract/e2equick不减。
