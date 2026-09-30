package goorderfulfillment

import "time"

// ---------- 库存登记 ----------

// RegisterInventoryRequest 向仓库台账登记（追加）实物库存。
// 重复调用为累加语义：同一仓库同一 SKU 的 OnHand 与 Available 都会增加。
type RegisterInventoryRequest struct {
	WarehouseID string
	SKU         string
	Quantity    int
}

// ---------- 订单确认 ----------

// ConfirmOrderRequest 确认一笔订单并为其生成跨仓拆分方案。
type ConfirmOrderRequest struct {
	// ExternalOrderID 外部订单号，作为订单幂等键。
	ExternalOrderID string
	Lines           []OrderLine
}

// Confirmation 订单确认结果。Reconfirmed 为 true 时表示命中幂等、返回的是既有方案。
type Confirmation struct {
	OrderID     string
	Version     int
	Reconfirmed bool
	Allocations []*Allocation
	Status      OrderStatus
}

// ---------- 拣货回执 ----------

// PickingReceiptItem 单个仓库对其名下某条分配的拣货结果。
type PickingReceiptItem struct {
	// AllocationID 目标分配 ID。
	AllocationID string
	// PickedQty 本次成功拣出的数量（增量）。
	PickedQty int
	// ShortageQty 本次确认的短缺数量（增量）：该部分永久无法由本仓提供。
	ShortageQty int
}

// PickingReceiptRequest 一批拣货回执（可来自不同仓库、不同订单行）。
type PickingReceiptRequest struct {
	Items []PickingReceiptItem
}

// AllocationView 回执处理后某条分配的最新状态快照。
type AllocationView struct {
	ID          string
	Version     int
	LineSKU     string
	WarehouseID string
	Quantity    int
	PickedQty   int
	ShippedQty  int
	ShortageQty int
	Status      AllocationStatus
}

// PickingReceiptResult 拣货回执处理结果。
type PickingReceiptResult struct {
	// Updated 被本次回执更新（或幂等命中）的分配快照。
	Updated []AllocationView
	// ReallocatedVersion 因短缺新生成的方案版本号；为 0 表示未发生重配。
	ReallocatedVersion int
	// Reallocated 新版本中的分配；重配因库存不足失败时为空，可在补货后调用 Reallocate 重试。
	Reallocated []*Allocation
	// ReallocateError 重配失败原因（如 ErrInsufficientInventory）；失败不影响已成功拣出数量的落账。
	ReallocateError error
	// Reallocations 本次回执涉及的每一个订单的重配结果，按订单号排序。
	// 一批回执可能跨多个订单：单值字段（ReallocatedVersion/Reallocated/
	// ReallocateError）只回传其中第一个订单的结果，其余订单统一看这里，
	// 避免“一个订单重配成功、另一个订单仍缺货”时失败信息被覆盖。
	Reallocations []OrderReallocation
}

// OrderReallocation 单个订单在某次拣货回执后的缺货重配结果。
type OrderReallocation struct {
	OrderID string
	// Version 新生成的方案版本号；重配失败（仍缺货）时为 0。
	Version int
	// Allocations 新版本中的分配；失败时为空，补货后可用 Reallocate 重试。
	Allocations []*Allocation
	// Err 重配失败原因（如 ErrInsufficientInventory）；成功时为 nil。
	Err error
}

// ---------- 缺货重配 ----------

// ReallocateRequest 为订单当前所有未覆盖缺口重新生成一版跨仓方案。
type ReallocateRequest struct {
	OrderID string
}

// ReallocateResult 重配结果。
type ReallocateResult struct {
	Version     int
	Allocations []*Allocation
}

// ---------- 发货 ----------

// ShipmentRequest 仓库确认发货。发货数量不得超过该分配“已拣未发”的数量，
// 因此同一件商品不可能被两个仓重复发出。
type ShipmentRequest struct {
	AllocationID string
	Quantity     int
}

// ---------- 取消 ----------

// CancelRequest 取消订单，仅释放尚未拣货的库存。
type CancelRequest struct {
	OrderID string
}

// ---------- 履约明细查询 ----------

// LineFulfillment 单个订单行的履约汇总。
type LineFulfillment struct {
	SKU      string
	Demanded int
	Picked   int
	Shipped  int
	Lost     int
	// Outstanding 尚未被任何活跃分配覆盖、也未拣出/发货的纯缺口。
	Outstanding int
}

// FulfillmentDetail 订单履约明细：状态、全部历史版本、全部发货记录与逐行汇总。
type FulfillmentDetail struct {
	OrderID       string
	Status        OrderStatus
	ActiveVersion int
	Lines         []OrderLine
	Versions      []*PlanVersion
	Shipments     []ShipmentRecord
	LineSummary   []LineFulfillment
	CreatedAt     time.Time
	UpdatedAt     time.Time
}

// InventorySnapshot 库存台账快照（返回副本，外部修改不影响内部状态）。
type InventorySnapshot struct {
	WarehouseID string
	SKU         string
	OnHand      int
	Available   int
	Reserved    int
	Picked      int
	Shipped     int
	Lost        int
}
