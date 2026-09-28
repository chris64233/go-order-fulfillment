package goorderfulfillment

// 领域标识类型，避免不同实体的字符串标识在调用方被混用。
type (
	// SKU 是商品编码。
	SKU string
	// WarehouseID 是仓库编码。
	WarehouseID string
	// OrderNumber 是外部订单号；同一订单号的重复确认走幂等/冲突逻辑。
	OrderNumber string
)

// OrderStatus 描述订单整体履约状态。
type OrderStatus string

const (
	// OrderConfirmed 已确认：已生成方案但尚未拣货（或仍有未拣货分配且无缺口）。
	OrderConfirmed OrderStatus = "confirmed"
	// OrderAwaitingStock 缺货等待：部分数量已拣出，剩余缺口暂时无库存可分配。
	OrderAwaitingStock OrderStatus = "awaiting_stock"
	// OrderPickedComplete 拣货完成：全部商品已拣出，但尚未全部发货。
	OrderPickedComplete OrderStatus = "picked_complete"
	// OrderPartiallyShipped 部分发货：已发出部分商品，仍有未发数量。
	OrderPartiallyShipped OrderStatus = "partially_shipped"
	// OrderShipped 已完成：订单全部数量均已发货。
	OrderShipped OrderStatus = "shipped"
	// OrderCancelled 已取消：取消时仅释放尚未拣货的库存，已拣货部分保留。
	OrderCancelled OrderStatus = "cancelled"
)

// PlanStatus 描述某一版拆分方案的状态。
type PlanStatus string

const (
	// PlanActive 活跃：仍存在未回执的分配行。
	PlanActive PlanStatus = "active"
	// PlanCompleted 完成：本版所有分配行都被仓库成功拣出。
	PlanCompleted PlanStatus = "completed"
	// PlanSuperseded 已替代：本版出现缺口且已为缺口生成新版方案。
	PlanSuperseded PlanStatus = "superseded"
	// PlanCancelled 已取消：订单取消时仍有未拣货分配行。
	PlanCancelled PlanStatus = "cancelled"
)

// AllocationStatus 描述方案中单个“仓-商品”分配行的状态。
type AllocationStatus string

const (
	// AllocPending 待拣货：库存已被占用，等待仓库回执。
	AllocPending AllocationStatus = "pending"
	// AllocPartial 部分拣货：仓库只拣出一部分，但该分配行尚未关闭，可继续补拣回执。
	AllocPartial AllocationStatus = "partial"
	// AllocPicked 已拣完：分配数量全部拣出。
	AllocPicked AllocationStatus = "picked"
	// AllocShortage 短缺关闭：仓库明确报告无法满足，未拣部分的占用已释放。
	AllocShortage AllocationStatus = "shortage"
	// AllocCancelled 取消关闭：订单取消，该未拣货分配的占用被释放。
	AllocCancelled AllocationStatus = "cancelled"
)

// OrderLine 是订单行。重复 SKU 行会在校验阶段合并。
type OrderLine struct {
	SKU      SKU
	Quantity int
}

// Stock 是单个仓库内单个 SKU 的库存账面。
//
// 库存恒等式：OnHand >= Reserved >= 0，可承诺量 ATP = OnHand - Reserved。
// 占用与释放只改 Reserved；OnHand 在两种情况下扣减——仓库成功拣出，
// 或仓库明确报告短缺（账实差异核销，找回后可重新 RegisterStock 入库）。
type Stock struct {
	WarehouseID WarehouseID
	SKU         SKU
	// OnHand 仓库现有实物总量（含已被占用、尚未拣出的部分）。
	OnHand int
	// Reserved 已被方案占用、尚未拣出或释放的数量。
	Reserved int
}

// Available 返回可承诺库存 ATP。
func (s Stock) Available() int {
	return s.OnHand - s.Reserved
}

// Allocation 是一版方案中“某个仓拣某个商品若干件”的分配行。
type Allocation struct {
	WarehouseID WarehouseID
	SKU         SKU
	// Quantity 本仓被分配的数量（拣货成功前一直占用库存）。
	Quantity int
	// PickedQuantity 已成功拣出并累计的数量。
	PickedQuantity int
	Status         AllocationStatus
}

// Open 表示该分配行是否仍在等待仓库拣货回执。
func (a Allocation) Open() bool {
	return a.Status == AllocPending || a.Status == AllocPartial
}

// AllocationPlan 是订单拆分方案的一个版本。每次缺货重配都会追加一个新版本，
// 旧版本永不删除，从而完整保留履约过程。
type AllocationPlan struct {
	OrderNumber OrderNumber
	// Version 从 1 开始单调递增。
	Version int
	// Allocations 本版方案的分配行，按仓库、SKU 排序。
	Allocations []*Allocation
	Status      PlanStatus
	// SupersededBy 当本版因缺口被新版替代时，指向首个承接缺口的版本号。
	SupersededBy int
}

// PickingLineItem 是仓库拣货回执中的单个商品条目。
type PickingLineItem struct {
	SKU SKU
	// PickedQuantity 本次实际拣出数量，可多次累计补拣。
	PickedQuantity int
	// Short 为 true 表示仓库明确声明：除已拣数量外，该商品的剩余数量无法拣出。
	// 系统会保留已拣数量、核销并释放剩余占用，然后为缺口生成下一版跨仓方案。
	// 为 false 时只累计拣货量，分配行保持开放，允许后续继续补拣。
	Short bool
}

// PickingReceipt 是仓库对某版方案某分配行的拣货回执。
type PickingReceipt struct {
	ReceiptID   string // 调用方提供的去重键，空表示不做回执级去重
	OrderNumber OrderNumber
	PlanVersion int
	WarehouseID WarehouseID
	Items       []PickingLineItem
}

// ShipmentItem 是发货确认中的单个商品条目。
type ShipmentItem struct {
	SKU      SKU
	Quantity int
}

// Shipment 是一条发货记录；同一仓对同一订单可分多次发货。
type Shipment struct {
	ShipmentID  string // 去重键，空表示不做发货级去重
	OrderNumber OrderNumber
	WarehouseID WarehouseID
	Items       []ShipmentItem
}

// Order 是订单聚合。
type Order struct {
	OrderNumber OrderNumber
	Lines       []OrderLine // 归一化后按 SKU 排序、无重复
	Status      OrderStatus
	// CurrentVersion 最新方案版本号；从未成功生成方案时为 0。
	CurrentVersion int
}
