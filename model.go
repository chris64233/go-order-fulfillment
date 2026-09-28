package goorderfulfillment

import "time"

// OrderStatus 订单整体履约状态。
type OrderStatus string

const (
	// StatusPending 已确认并生成方案，等待仓库拣货。
	StatusPending OrderStatus = "pending"
	// StatusInFulfillment 已有部分行完成拣货或部分发货，仍存在未结清缺口。
	StatusInFulfillment OrderStatus = "in_fulfillment"
	// StatusShipped 订单全部需求数量均已发货，履约完成（终态）。
	StatusShipped OrderStatus = "shipped"
	// StatusCancelled 订单被取消。取消后不再允许生成新分配；
	// 取消前已拣出的部分仍可发货（部分取消）。
	StatusCancelled OrderStatus = "cancelled"
)

// AllocationStatus 单条仓库分配（方案明细行）的状态。
type AllocationStatus string

const (
	// AllocActive 分配生效中，占用仓库库存、等待拣货回执。
	AllocActive AllocationStatus = "active"
	// AllocPickedComplete 该分配全部数量拣货成功。
	AllocPickedComplete AllocationStatus = "picked_complete"
	// AllocShortageClosed 仓库报告部分短缺后关闭：成功拣出的数量被保留，
	// 短缺数量被核销，剩余缺口由新版方案承接。
	AllocShortageClosed AllocationStatus = "shortage_closed"
	// AllocCancelled 订单取消时释放：该分配尚未拣出的库存被退回可用库存。
	AllocCancelled AllocationStatus = "cancelled"
)

func (s AllocationStatus) closed() bool {
	return s != AllocActive
}

// Inventory 单个仓库、单个 SKU 的库存台账。
// 恒等关系：OnHand == Available + Reserved + Picked + Shipped + Lost。
type Inventory struct {
	WarehouseID string
	SKU         string
	// OnHand 仓库实物账面库存（含可用、已占、已拣、已发、已核销损耗）。
	OnHand int
	// Available 可用于新分配的库存。
	Available int
	// Reserved 已被活跃分配占用、尚未拣货的库存。
	Reserved int
	// Picked 已拣出待发货的库存。
	Picked int
	// Shipped 已发货出库的库存。
	Shipped int
	// Lost 拣货时确认短缺、永久核销的库存。
	Lost int
}

// OrderLine 订单行：一个 SKU 的需求数量。
type OrderLine struct {
	SKU      string
	Quantity int
}

// Allocation 某一版方案中，把某订单行的一部分数量分配给某个仓库。
type Allocation struct {
	// ID 形如 <orderExternalID>#v<version>-<lineSKU>-<warehouseID>，全局唯一。
	ID          string
	OrderID     string
	Version     int
	LineSKU     string
	WarehouseID string
	// Quantity 本分配承担的数量。
	Quantity int
	// PickedQty 累计成功拣出数量。
	PickedQty int
	// ShippedQty 累计已由该仓库发出的数量。
	ShippedQty int
	// ShortageQty 仓库报告的短缺（永久缺口）数量。
	ShortageQty int
	Status      AllocationStatus
	CreatedAt   time.Time
	UpdatedAt   time.Time
}

// outstanding 该分配尚未得到拣货回执的数量。
func (a *Allocation) outstanding() int {
	return a.Quantity - a.PickedQty - a.ShortageQty
}

// PlanVersion 一版跨仓拆分方案。重配会生成新版本，旧版本完整保留用于审计。
type PlanVersion struct {
	Version     int
	OrderID     string
	Allocations []*Allocation
	CreatedAt   time.Time
	// Note 记录该版本的产生原因，如 "confirm"、"reallocate after shortage"。
	Note string
}

// ShipmentRecord 一条发货记录：某仓库把某订单行的某数量实际发出。
type ShipmentRecord struct {
	ID           string
	OrderID      string
	AllocationID string
	LineSKU      string
	WarehouseID  string
	Quantity     int
	CreatedAt    time.Time
}

// Order 订单聚合。
type Order struct {
	ID        string // 即外部订单号
	Lines     []OrderLine
	lineIndex map[string]int
	// ContentFingerprint 订单内容（SKU+数量）的规范化指纹，用于幂等与冲突检测。
	ContentFingerprint string
	Status             OrderStatus
	// ActiveVersion 当前有效方案版本号；0 表示尚无活跃方案（已取消或已发完）。
	ActiveVersion int
	// versions 该订单全部历史方案版本，永不删除，按版本号有序。
	versions  []*PlanVersion
	CreatedAt time.Time
	UpdatedAt time.Time
}

func orderVersions(o *Order) []*PlanVersion { return o.versions }

func (o *Order) setVersions(v []*PlanVersion) { o.versions = v }
