# go-order-fulfillment

订单跨仓拆分履约与缺货重配的 Go 领域库（无第三方依赖，Go 1.23+）。

## 能力概览

1. **库存登记**：按“仓库 × SKU”登记实物库存，维护
   `OnHand（实物）>= Reserved（占用）>= 0`，可承诺量 `ATP = OnHand - Reserved`。
2. **订单确认 / 跨仓拆分**：按各仓 ATP 从低到高依次分配，方案可跨仓；
   **每个订单行必须完整满足**，任一行不足则整体拒绝，不创建订单、不留任何占用。
3. **并发安全与幂等**：所有写操作在同一把互斥锁内原子完成，并发确认不会超卖；
   相同外部订单号 + 相同商品/数量重复确认返回首次的原方案；
   订单号相同但内容变化返回 `ErrOrderConflict`。
4. **拣货回执 / 缺货重配**：
   - 拣货数量可多次累计补拣；只有真正拣出才扣减 `OnHand` 与 `Reserved`。
   - 条目显式带 `Short: true` 表示声明短缺：已拣数量保留，剩余缺口核销并释放，
     随后系统自动按当前各仓 ATP 生成**下一版跨仓方案**（版本号递增，旧版保留）。
   - ATP 暂时不足时订单进入 `awaiting_stock`，补货后可调用 `Reallocate` 再试。
   - **旧方案的迟到回执一律以 `ErrStaleReceipt` 拒绝**，不能覆盖新分配；
     发货量又受拣货量约束，因此同一件商品不可能被两个仓重复发出。
5. **发货**：只能由真正拣货的仓库、在“已拣未发”数量内发货，支持分批发货与
   `ShipmentID` 幂等。
6. **取消**：只释放尚未拣货的占用；已拣出部分是履约事实不回滚。取消为终态，
   与缺货重配、发货并发发生时，订单状态 / 库存占用 / 发货记录始终一致。
7. **履约明细查询**：返回订单头、**每一版方案**、按 SKU 的需求/已拣/已发进度
   以及全部发货记录。
8. **持久化**：可选的 JSON 文件快照——每次写操作成功后在锁内原子写盘
   （临时文件 + rename），重启无损恢复，回执/发货幂等集合一并保存。

## 快速开始

```go
svc := goorderfulfillment.NewService()

// 1) 登记库存
_ = svc.RegisterStock("WH-A", "SKU-X", 5)
_ = svc.RegisterStock("WH-B", "SKU-X", 3)

// 2) 确认订单（X 需 7 件 -> 跨 A(5) + B(2)）
plan, err := svc.ConfirmOrder("ORD-1", []goorderfulfillment.OrderLine{
    {SKU: "SKU-X", Quantity: 7},
})
// err == ErrInsufficientStock 表示全渠道都凑不齐，调用整体未生效

// 3) 仓库回执：A 只拣到 3 件并声明短缺 -> 缺口自动重配
res, _ := svc.ReportPicking(goorderfulfillment.PickingReceipt{
    ReceiptID: "wha-1", OrderNumber: "ORD-1", PlanVersion: plan.Version,
    WarehouseID: "WH-A",
    Items: []goorderfulfillment.PickingLineItem{
        {SKU: "SKU-X", PickedQuantity: 3, Short: true},
    },
})
// res.NewPlan 是为缺口生成的下一版方案；ATP 不足时为 nil 且状态 awaiting_stock

// 4) 补货后可手动再配
_ = svc.RegisterStock("WH-B", "SKU-X", 4)
v2, _ := svc.Reallocate("ORD-1")

// 5) 拣满后发货（可分批）
_, _ = svc.ConfirmShipment(goorderfulfillment.Shipment{
    ShipmentID: "sh-1", OrderNumber: "ORD-1", WarehouseID: "WH-B",
    Items: []goorderfulfillment.ShipmentItem{{SKU: "SKU-X", Quantity: 7}},
})

// 6) 查询完整履约明细（含全部历史方案版本）
detail, _ := svc.GetFulfillment("ORD-1")
```

### 持久化到文件

```go
// 文件不存在时返回空服务并自动开始快照；已存在则恢复全部状态
svc, existed, err := goorderfulfillment.NewServiceFromFile("data/state.json")
// 之后每次写操作都会原子覆盖该文件；也可对内存服务事后开启：
// svc.EnableFileSnapshot("data/state.json")
```

## API 一览

| 方法 | 说明 |
| --- | --- |
| `RegisterStock(warehouse, sku, qty)` | 增量登记/补充库存 |
| `GetStock` / `ListStock` | 库存账面与 ATP 查询 |
| `ConfirmOrder(orderNo, lines)` | 确认订单并生成 v1 跨仓方案（幂等/冲突/全有全无） |
| `ReportPicking(receipt)` | 拣货回执：累计补拣、显式短缺、自动重配 |
| `Reallocate(orderNo)` | 为当前缺口主动生成下一版方案 |
| `ConfirmShipment(shipment)` | 发货确认（受已拣未发数量约束，幂等） |
| `CancelOrder(orderNo)` | 取消，仅释放未拣货占用（幂等终态） |
| `GetOrder` / `GetPlan(no, ver)` | 订单与指定版本方案查询（`ver<=0` 为最新版） |
| `GetFulfillment(orderNo)` | 完整履约明细：订单、全部版本、行进度、发货记录 |

主要错误哨兵：`ErrInsufficientStock`、`ErrOrderConflict`、`ErrStaleReceipt`、
`ErrOverPicked`、`ErrOverShipped`、`ErrReceiptDuplicate`、
`ErrShipmentDuplicate`、`ErrOrderTerminal`、`ErrNothingToReallocate`、
`ErrPlanNotFound`、`ErrInvalidInput`（均可用 `errors.Is` 判定）。

## 订单与方案状态

- 订单：`confirmed → awaiting_stock → picked_complete → partially_shipped → shipped`，
  任意阶段可进入终态 `cancelled`（已全部发货不可取消）。
- 方案版本：`active → completed`（整版拣满）/ `superseded`（缺口由新版承接）/
  `cancelled`（订单取消时仍开放）。
- 分配行：`pending → partial → picked`，或 `shortage` / `cancelled` 关闭。

## 一致性设计要点

- **全有全无**：拆分方案先在“局部草稿”上试算全部订单行，任一 SKU 不足直接返回，
  不触碰真实库存；全部满足后才一次性提交占用。
- **单一事务边界**：服务用一把互斥锁串行化全部读写，快照也在持锁期间落盘，
  外部永远观察不到“库存已占但订单没建”“已取消却还能发货”之类的中间态。
- **缺口不重不漏**：缺口 = 订单需求 − 开放分配整件数（含其已拣部分）
  − 已关闭分配的成功拣出量；已关闭的短缺/取消行不再计入在途。
- **短缺核销**：仓库声明拿不出货的数量同时从 `OnHand` 核销，避免缺口立刻
  回流到同一仓库；货物找回时用 `RegisterStock` 重新入库即可。

## 测试

```bash
go test -race -count=1 ./...
```

覆盖：跨仓拆分与整体拒绝、幂等/冲突、50 并发订单不超卖、同单并发确认、
部分短缺自动重配、迟到回执拒绝、重复发货防护、取消仅释放未拣货、
取消/重配/发货高并发混合压测下的库存与发货不变量、每版方案持久化与重启恢复。
