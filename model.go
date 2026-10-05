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
	// PackedQty 已拣出且已装入待出库包裹、尚未随包裹出库的数量。
	// 该部分不能再通过 Ship 直接发货，只能由包裹出库扣减。
	PackedQty int
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

// PackageStatus 包裹状态。
type PackageStatus string

const (
	// PackagePending 已装箱、尚未出库，可参与合并。
	PackagePending PackageStatus = "pending"
	// PackageShipped 已出库，不可再被合并。
	PackageShipped PackageStatus = "shipped"
	// PackageInTransit 承运中，不可再被合并。
	PackageInTransit PackageStatus = "in_transit"
	// PackageMerged 已被合并进新包裹。记录永久保留用于审计，不可删除、不可恢复。
	PackageMerged PackageStatus = "merged"
)

// mergeable 包裹是否可参与合并：仅“尚未出库”的包裹可以。
func (s PackageStatus) mergeable() bool { return s == PackagePending }

// packageItem 包裹内按来源分配记录的商品明细（内部结构），
// 包裹出库时据此精确扣减对应分配与仓库库存。
type packageItem struct {
	AllocationID string
	SKU          string
	Quantity     int
}

// Package 包裹：一张订单下、同一目的地、同一承运约束的一组已拣商品。
type Package struct {
	// ID 形如 <orderExternalID>#pkg-<seq>，全局唯一、按创建顺序递增。
	ID          string
	OrderID     string
	Destination string
	// Carrier 承运约束（承运商/运输方式），合并时各包裹必须一致。
	Carrier string
	// items 按来源分配记录的内部明细，合并时整体转移给新包裹。
	items []packageItem
	// Items 聚合后的商品明细（按 SKU 升序）。
	Items  []OrderLine
	Status PackageStatus
	// MergedInto 仅当 Status 为 merged 时有效：指向合并产生的新包裹 ID。
	MergedInto string
	// MergeID 产生本包裹（新包裹）或消费本包裹（旧包裹）的合并号。
	MergeID   string
	CreatedAt time.Time
	UpdatedAt time.Time
}

// MergeRecord 一次包裹合并的审计记录，按合并号持久化，用于幂等重放与历史查询。
type MergeRecord struct {
	MergeID string
	OrderID string
	// SourcePackageIDs 被合并的原包裹 ID，升序保存。
	SourcePackageIDs []string
	NewPackageID     string
	Destination      string
	Carrier          string
	Items            []OrderLine
	// fingerprint 合并申请内容（订单、包裹集合、目的地、承运约束、商品明细）的规范化指纹。
	fingerprint string
	// seq 合并记录的全局创建序号，保证查询排序稳定。
	seq       int
	CreatedAt time.Time
}
