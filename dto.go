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
	// Packages 该订单全部包裹（含已合并的历史包裹），按包裹 ID 升序，排序稳定。
	Packages []PackageView
	// Merges 该订单全部合并审计记录，按合并发生顺序排列，排序稳定。
	Merges    []MergeAudit
	CreatedAt time.Time
	UpdatedAt time.Time
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

// ---------- 装箱（生成包裹） ----------

// PackItem 从某条分配的“已拣未发未装箱”数量中取出若干件装入包裹。
type PackItem struct {
	AllocationID string
	Quantity     int
}

// PackItemsRequest 把一张订单下若干分配的已拣商品装成一个待出库包裹。
// 同一包裹内所有商品共享目的地与承运约束。
type PackItemsRequest struct {
	OrderID     string
	Destination string
	Carrier     string
	Items       []PackItem
}

// PackageView 包裹的对外快照。
type PackageView struct {
	ID          string
	OrderID     string
	Destination string
	Carrier     string
	Items       []OrderLine
	Status      PackageStatus
	// MergedInto 仅当 Status 为 merged 时有效：指向合并产生的新包裹 ID。
	MergedInto string
	// MergeID 产生（新包裹）或消费（旧包裹）本包裹的合并号。
	MergeID   string
	CreatedAt time.Time
	UpdatedAt time.Time
}

// ---------- 包裹合并 ----------

// MergePackagesRequest 包裹合并申请。合并号为幂等键：
// 同一合并号 + 相同内容重放返回原结果；包裹集合、目的地或商品明细变化返回
// ErrMergeConflict；承运约束变化返回 ErrMergeCarrierConflict。
type MergePackagesRequest struct {
	// MergeID 合并号（幂等键）。
	MergeID string
	// OrderID 被合并包裹所属订单；集合中任何包裹不属于该订单即整单拒绝。
	OrderID string
	// PackageIDs 待合并的原包裹集合（至少 2 个，全部必须尚未出库）。
	PackageIDs []string
	// Destination 目的地；集合中任何包裹目的地不同即整单拒绝。
	Destination string
	// Carrier 承运约束；集合中任何包裹承运约束不同即整单拒绝。
	Carrier string
	// Items 申请声明的商品明细，必须与各原包裹商品合计完全一致。
	Items []OrderLine
}

// MergePackagesResult 合并确认结果。Replayed 为 true 表示命中幂等、返回原结果。
type MergePackagesResult struct {
	MergeID string
	OrderID string
	// NewPackage 合并产生的唯一新包裹。
	NewPackage PackageView
	// MergedPackages 被合并的原包裹快照（状态已为 merged），按包裹 ID 升序。
	MergedPackages []PackageView
	Replayed       bool
}

// MergeAudit 合并审计记录（查询用）。
type MergeAudit struct {
	MergeID          string
	OrderID          string
	SourcePackageIDs []string
	NewPackageID     string
	Destination      string
	Carrier          string
	Items            []OrderLine
	CreatedAt        time.Time
}

// ---------- 包裹出库 / 承运 ----------

// PackageOutboundRequest 包裹出库回执：pending → shipped，
// 库存按包裹明细从 Picked 结转至 Shipped。已合并/已出库/承运中的包裹
// 收到回执一律返回 ErrStalePackage，不能恢复或改变其状态。
type PackageOutboundRequest struct {
	PackageID string
}

// PackageTransitRequest 承运揽收回执：shipped → in_transit。
type PackageTransitRequest struct {
	PackageID string
}
