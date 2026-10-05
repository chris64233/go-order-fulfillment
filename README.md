# go-order-fulfillment

订单跨仓拆分履约领域服务（Go，零依赖、并发安全、内存持久化每版方案）。

支持：库存登记、订单确认（跨仓拆分 + 幂等/冲突检测）、仓库拣货回执、部分缺货自动重配、
发货确认、取消、包裹装箱与合并、包裹出库/承运回执、履约明细查询。

## 快速开始

```go
svc := goorderfulfillment.NewFulfillmentService()

// 1. 登记各仓库存（累加语义）
svc.RegisterInventory(goorderfulfillment.RegisterInventoryRequest{WarehouseID: "W1", SKU: "A", Quantity: 7})
svc.RegisterInventory(goorderfulfillment.RegisterInventoryRequest{WarehouseID: "W2", SKU: "A", Quantity: 3})

// 2. 确认订单：自动生成跨仓拆分方案并原子占用库存
conf, _ := svc.ConfirmOrder(goorderfulfillment.ConfirmOrderRequest{
    ExternalOrderID: "ORDER-1",
    Lines:           []goorderfulfillment.OrderLine{{SKU: "A", Quantity: 10}},
})
// conf.Allocations => W1:7, W2:3

// 3. 拣货回执：W1 拣齐 7；W2 短缺 3
res, _ := svc.PickingReceipt(goorderfulfillment.PickingReceiptRequest{Items: []goorderfulfillment.PickingReceiptItem{
    {AllocationID: conf.Allocations[0].ID, PickedQty: 7},
    {AllocationID: conf.Allocations[1].ID, ShortageQty: 3},
}})
// res.ReallocatedVersion == 2：系统已用其他仓库的可用库存为缺口 3 件生成新版方案

// 4. 发货（只能发“已拣未发”的数量）
svc.Ship(goorderfulfillment.ShipmentRequest{AllocationID: conf.Allocations[0].ID, Quantity: 7})

// 5. 查询完整履约明细（含每一版历史方案与全部发货记录）
detail, _ := svc.GetFulfillmentDetail("ORDER-1")

// 6. 取消（仅释放尚未拣货的预留）
svc.Cancel(goorderfulfillment.CancelRequest{OrderID: "ORDER-1"})

// 7. 装箱：把已拣商品装成待出库包裹（同一目的地 + 同一承运约束）
pkg1, _ := svc.PackItems(goorderfulfillment.PackItemsRequest{
    OrderID: "ORDER-1", Destination: "DEST-1", Carrier: "CARRIER-A",
    Items: []goorderfulfillment.PackItem{{AllocationID: conf.Allocations[0].ID, Quantity: 7}},
})

// 8. 包裹合并：多个尚未出库、目的地相同的包裹合并为一个新包裹
merge, _ := svc.MergePackages(goorderfulfillment.MergePackagesRequest{
    MergeID: "MERGE-1", OrderID: "ORDER-1",
    PackageIDs: []string{pkg1.ID, pkg2.ID},
    Destination: "DEST-1", Carrier: "CARRIER-A",
    Items: []goorderfulfillment.OrderLine{{SKU: "A", Quantity: 10}},
})
// merge.NewPackage 是唯一的新包裹；原包裹保留并标记为 merged

// 9. 包裹出库 / 承运揽收回执
svc.PackageOutbound(goorderfulfillment.PackageOutboundRequest{PackageID: merge.NewPackage.ID})
svc.PackageTransit(goorderfulfillment.PackageTransitRequest{PackageID: merge.NewPackage.ID})
```

## API

| 方法 | 说明 |
| --- | --- |
| `RegisterInventory` | 登记/追加仓库实物库存 |
| `GetInventory` | 查询单仓单 SKU 台账 |
| `ConfirmOrder` | 确认订单，生成 v1 跨仓方案并占用库存 |
| `PickingReceipt` | 提交一批拣货回执（成功数/短缺数），短缺时自动重配 |
| `ReceiptPick` | 便捷方法：单条分配“全部拣出”回执 |
| `Reallocate` | 补货后手动为剩余缺口重配（自动重配缺货失败时使用） |
| `Ship` | 仓库发货确认 |
| `Cancel` | 取消订单，只释放未拣货库存 |
| `GetFulfillmentDetail` | 订单状态、逐行汇总、全部版本方案、全部发货记录 |
| `PackItems` | 把已拣未发的商品装成待出库包裹（固定目的地与承运约束） |
| `MergePackages` | 包裹合并申请：多个未出库、同目的地包裹合并为一个新包裹 |
| `PackageOutbound` | 包裹出库回执（pending → shipped），库存 Picked → Shipped |
| `PackageTransit` | 承运揽收回执（shipped → in_transit） |

## 核心规则

### 1. 订单确认：跨仓拆分、整单原子

- 按各仓可用库存（可用量降序、仓库 ID 升序，结果确定）为**全部**订单行生成方案，单行可拆给多个仓。
- **任何一行不足，整单拒绝**（`ErrInsufficientInventory`）：先纯计算完整方案、再统一占用库存，
  因此不会留下其他行已经占用的库存。
- 库存台账恒等式：`OnHand = Available + Reserved + Picked + Shipped + Lost`。

### 2. 幂等与冲突

- 相同外部订单号 + 相同内容（SKU 与数量的规范化指纹）重复确认：返回原方案（`Reconfirmed=true`），不重复占库存。
- 相同订单号但商品或数量变化：返回 `ErrOrderConflict`，原方案不变。
- 全部公开操作在同一把互斥锁内串行完成“检查 + 写入”，并发确认不会超卖。

### 3. 拣货回执与缺货重配

- 成功拣出：`Reserved → Picked`，**数量永久保留**，即使同行其他仓短缺也不受影响。
- 部分短缺：短缺部分 `Reserved → Lost` 核销，该分配关闭；系统在同一临界区内
  为剩余缺口（需求量 − 已拣 − 活跃分配待拣量）生成**下一版跨仓方案**并持久化（v1、v2… 全部保留）。
- 自动重配时若全局仍缺货：已拣数量照样落账，缺口悬空；补货后调用 `Reallocate` 重试。
- **迟到回执防护**：已关闭（拣齐/短缺/取消）的分配收到任何回执一律返回 `ErrStaleAllocation`，
  旧方案回执无法覆盖新分配。
- 一批回执先整批校验（不存在、超量 `ErrReceiptOverflow`、批内重复）再落账，保证批原子性。

### 4. 发货防重

- 发货数量不得超过该分配“已成功拣出且尚未发出”的数量（`ErrShipExceedsPicked`）。
- 货物只能从真正拣到它的那个仓发出，**同一件商品不可能被两个仓重复发出**。

### 5. 取消与并发一致性

- 取消只把仍活跃分配的未拣货预留（`Reserved → Available`）释放；已拣出部分留在 `Picked`，仍可发货。
- 取消后不允许重配；针对已释放分配的迟到拣货回执会被拒绝，库存不会被重新占用。
- 取消、缺货重配、发货确认由同一把锁互斥，订单状态、库存占用、发货记录在任何交错下都作为整体一致。

### 6. 包裹装箱与合并

- **装箱**：`PackItems` 从分配的“已拣、未发、未装箱”数量中取材，生成 `pending` 包裹；
  已装箱数量计入分配的 `PackedQty`，不能再通过 `Ship` 直接发货，杜绝重复发货。
- **合并申请固定四要素**：订单、包裹集合、商品明细、承运约束。集合中存在任何不可合并包裹
  （不存在、不属于本订单、已出库、承运中、已合并、目的地不同、承运约束不符）时**整单拒绝**，
  不产生任何状态变化；声明的商品明细必须与原包裹合计完全一致（`ErrMergeItemMismatch`）。
- **合并确认只产生一个新包裹**：原包裹记录永久保留并标记为 `merged`（`MergedInto` 指向新包裹），
  历史不删除；商品明细按来源分配整体转移给新包裹。
- **幂等与冲突**：同一合并号 + 相同内容重放返回原结果（`Replayed=true`）；
  包裹集合、目的地或商品明细变化返回 `ErrMergeConflict`；
  仅承运约束变化返回 `ErrMergeCarrierConflict`，明确阻断原因。
- **旧包裹回执防护**：已合并/已出库/承运中的包裹收到出库或揽收回执一律返回
  `ErrStalePackage`，不能恢复已合并记录，也不会重复扣减库存。
- **并发一致性**：拣货、装箱、合并、取消、出库在同一把锁内串行，
  库存扣减（`Picked → Shipped`）与包裹状态迁移作为整体一致；
  不变量：全部待出库包裹商品总量 == 全部分配 `PackedQty` 合计。
- **查询**：`GetFulfillmentDetail` 返回全部包裹（含已合并历史，按包裹 ID 升序）与
  合并审计记录（按合并发生顺序），排序稳定、深拷贝返回。

## 状态模型

- 订单：`pending`（方案已建）→ `in_fulfillment`（部分拣货/发货/悬空缺口）→ `shipped`（全部需求已发）；
  随时可 → `cancelled`（部分取消时已拣部分仍允许发完，发完后归一为 `shipped`）。
- 分配：`active` → `picked_complete` / `shortage_closed` / `cancelled`，关闭后不可再回执。
- 包裹：`pending`（待出库，可合并）→ `shipped`（已出库）→ `in_transit`（承运中）；
  `pending` → `merged`（被合并，终态，记录保留）。仅 `pending` 可参与合并。

## 测试

```bash
go test -race -count=1 ./...
```

覆盖：跨仓拆分、整单原子拒绝、幂等/冲突、30 并发抢 10 件库存的防超卖、
部分短缺自动重配、迟到/重复回执拒绝、批原子性、补货后手动重配、发货防重、
取消只释放未拣货、40 订单并发交错（取消/重配/发货/查询）后的全局账实审计、
库存恒等式与防重复发货校验；包裹合并的整单拒绝（部分不可合并）、幂等重放与
集合/目的地/承运约束冲突、旧包裹重复回执防护、装箱超量与装箱后直发拦截、
拣货/合并/取消并发竞争一致性、包裹与合并审计的历史查询稳定性。

## 持久化说明

当前版本为内存实现：所有方案版本、分配、发货记录保存在 `FulfillmentService` 进程内，
进程重启不保留。服务边界（请求/返回 DTO 与哨兵错误）已按可替换仓储设计，
后续可在 `FulfillmentService` 内部把 map 替换为数据库实现而不改动公开 API。
