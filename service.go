package goorderfulfillment

import (
	"crypto/sha256"
	"encoding/hex"
	"sort"
	"sync"
)

// Service 是订单履约领域服务。
//
// 所有写操作都在同一把互斥锁下完成：订单状态、库存占用、方案版本与发货记录
// 的任何一步联动都对外呈现为一个原子整体，因此并发确认不会超卖，
// 取消、缺货重配与发货确认交错发生时也不会出现中间态。
type Service struct {
	mu sync.Mutex
	st *store
	// persist 在每次成功写操作（持锁状态）后被调用，用于把全量状态快照落盘；
	// 为 nil 表示纯内存运行。
	persist func(s *store) error
}

// NewService 创建纯内存履约服务。
func NewService() *Service {
	return &Service{st: newStore()}
}

// ---------- 库存登记 ----------

// RegisterStock 登记/补充库存：为指定仓库的 SKU 增量增加实物库存。
// 首次登记某个“仓-SKU”等价于入库建档；不允许通过负数绕过库存恒等式
// （OnHand >= Reserved），需要扣减请走取消/拣货流程。
func (svc *Service) RegisterStock(w WarehouseID, sku SKU, quantity int) error {
	if w == "" || sku == "" || quantity <= 0 {
		return ErrInvalidInput
	}
	svc.mu.Lock()
	defer svc.mu.Unlock()

	st := svc.st.getStock(w, sku, true)
	st.OnHand += quantity
	return svc.saved()
}

// GetStock 查询单个“仓-SKU”的库存账面；不存在返回 ErrInvalidInput 之外的
// 零值库存视图（不报错），便于调用方盘点。
func (svc *Service) GetStock(w WarehouseID, sku SKU) Stock {
	svc.mu.Lock()
	defer svc.mu.Unlock()
	if st := svc.st.getStock(w, sku, false); st != nil {
		return *st
	}
	return Stock{WarehouseID: w, SKU: sku}
}

// StockView 是一行库存视图。
type StockView struct {
	WarehouseID WarehouseID
	SKU         SKU
	OnHand      int
	Reserved    int
	Available   int
}

// ListStock 按仓库、SKU 升序返回全部库存视图。
func (svc *Service) ListStock() []StockView {
	svc.mu.Lock()
	defer svc.mu.Unlock()
	var out []StockView
	ws := svc.st.knownWarehouses()
	for _, w := range ws {
		skus := make([]SKU, 0, len(svc.st.stock[w]))
		for sku := range svc.st.stock[w] {
			skus = append(skus, sku)
		}
		sort.Slice(skus, func(i, j int) bool { return skus[i] < skus[j] })
		for _, sku := range skus {
			st := svc.st.stock[w][sku]
			out = append(out, StockView{w, sku, st.OnHand, st.Reserved, st.Available()})
		}
	}
	return out
}

// ---------- 订单确认与跨仓拆分 ----------

// ConfirmOrder 按外部订单号确认订单，并基于各仓可用库存（ATP）生成跨仓拆分方案。
//
// 规则：
//   - 每个订单行必须被完整满足才提交；任意一行不足则整体拒绝，不占用任何库存，
//     也不会创建订单。
//   - 相同订单号 + 相同内容重复确认：幂等返回首次确认生成的原方案。
//   - 相同订单号但商品或数量变化：返回 ErrOrderConflict。
func (svc *Service) ConfirmOrder(orderNo OrderNumber, lines []OrderLine) (*AllocationPlan, error) {
	norm, err := normalizeLines(lines)
	if err != nil {
		return nil, err
	}
	if orderNo == "" {
		return nil, ErrInvalidInput
	}

	svc.mu.Lock()
	defer svc.mu.Unlock()

	if existing := svc.st.orders[orderNo]; existing != nil {
		if contentFingerprint(norm) != contentFingerprint(existing.Lines) {
			return nil, ErrOrderConflict
		}
		// 幂等重放：返回首次确认时的原方案（第 1 版）。
		return svc.st.getPlan(orderNo, 1)
	}

	plan, err := svc.buildPlanLocked(orderNo, norm, 1)
	if err != nil {
		// buildPlanLocked 失败时不写任何状态，天然满足整体回滚。
		return nil, err
	}

	o := &Order{
		OrderNumber:    orderNo,
		Lines:          norm,
		Status:         OrderConfirmed,
		CurrentVersion: 1,
	}
	svc.st.orders[orderNo] = o
	svc.st.appendPlan(plan)
	if err := svc.saved(); err != nil {
		// 快照失败同样整体回滚，调用方可以安全重试。
		delete(svc.st.orders, orderNo)
		svc.releasePlanLocked(plan)
		delete(svc.st.plans, orderNo)
		return nil, err
	}
	return clonePlan(plan), nil
}

// buildPlanLocked 在持锁状态下为需求行计算并“占用”一个跨仓方案。
//
// 分配顺序是确定的：按 SKU、再按仓库 ID 升序，依次吃掉每个仓的 ATP。
// 计算阶段只在局部记账，任意一个 SKU 不足即返回 ErrInsufficientStock，
// 不动任何真实库存；全部满足后才一次性提交占用。
func (svc *Service) buildPlanLocked(orderNo OrderNumber, lines []OrderLine, version int) (*AllocationPlan, error) {
	type usedKey struct {
		w   WarehouseID
		sku SKU
	}
	used := map[usedKey]int{}
	var allocs []*Allocation

	for _, line := range lines {
		need := line.Quantity
		for _, w := range svc.st.knownWarehouses() {
			if need == 0 {
				break
			}
			st := svc.st.getStock(w, line.SKU, false)
			if st == nil {
				continue
			}
			avail := st.OnHand - st.Reserved - used[usedKey{w, line.SKU}]
			if avail <= 0 {
				continue
			}
			take := need
			if take > avail {
				take = avail
			}
			allocs = append(allocs, &Allocation{
				WarehouseID: w,
				SKU:         line.SKU,
				Quantity:    take,
				Status:      AllocPending,
			})
			used[usedKey{w, line.SKU}] += take
			need -= take
		}
		if need > 0 {
			return nil, ErrInsufficientStock
		}
	}

	// 全部满足：提交占用。
	for _, a := range allocs {
		st := svc.st.getStock(a.WarehouseID, a.SKU, true)
		st.Reserved += a.Quantity
	}
	sortAllocations(allocs)
	return &AllocationPlan{
		OrderNumber: orderNo,
		Version:     version,
		Allocations: allocs,
		Status:      PlanActive,
	}, nil
}

// releasePlanLocked 释放整版方案尚未拣货的占用（回滚/取消辅助）。
// 已拣货部分的占用在拣货时就已扣减，这里只会遇到未拣货数量。
func (svc *Service) releasePlanLocked(p *AllocationPlan) {
	for _, a := range p.Allocations {
		if a.Open() {
			svc.adjustReserved(a.WarehouseID, a.SKU, -(a.Quantity - a.PickedQuantity))
		}
	}
}

func (svc *Service) adjustReserved(w WarehouseID, sku SKU, delta int) {
	st := svc.st.getStock(w, sku, false)
	if st == nil {
		return
	}
	st.Reserved += delta
	if st.Reserved < 0 {
		st.Reserved = 0
	}
}

// ---------- 查询 ----------

// GetOrder 返回订单快照；不存在返回 ErrOrderNotFound。
func (svc *Service) GetOrder(orderNo OrderNumber) (Order, error) {
	svc.mu.Lock()
	defer svc.mu.Unlock()
	o, err := svc.st.getOrder(orderNo)
	if err != nil {
		return Order{}, err
	}
	cp := *o
	cp.Lines = append([]OrderLine(nil), o.Lines...)
	return cp, nil
}

// GetPlan 返回指定版本方案；version <= 0 表示最新版本。
func (svc *Service) GetPlan(orderNo OrderNumber, version int) (*AllocationPlan, error) {
	svc.mu.Lock()
	defer svc.mu.Unlock()
	if _, err := svc.st.getOrder(orderNo); err != nil {
		return nil, err
	}
	p, err := svc.st.getPlan(orderNo, version)
	if err != nil {
		return nil, err
	}
	return clonePlan(p), nil
}

// LineProgress 是订单行维度的履约进度。
type LineProgress struct {
	SKU      SKU
	Required int
	Picked   int
	Shipped  int
}

// FulfillmentDetail 是履约明细：订单头、全部历史方案版本、行进度、发货记录。
type FulfillmentDetail struct {
	Order     Order
	Plans     []*AllocationPlan
	Lines     []LineProgress
	Shipments []*Shipment
}

// GetFulfillment 返回订单完整履约明细（持久化的每一版方案都会列出）。
func (svc *Service) GetFulfillment(orderNo OrderNumber) (*FulfillmentDetail, error) {
	svc.mu.Lock()
	defer svc.mu.Unlock()
	o, err := svc.st.getOrder(orderNo)
	if err != nil {
		return nil, err
	}

	detail := &FulfillmentDetail{Order: *o}
	detail.Order.Lines = append([]OrderLine(nil), o.Lines...)
	for _, p := range svc.st.allPlans(orderNo) {
		detail.Plans = append(detail.Plans, clonePlan(p))
	}
	for _, sh := range svc.st.shipments[orderNo] {
		detail.Shipments = append(detail.Shipments, cloneShipment(sh))
	}

	picked := map[SKU]int{}
	shipped := map[SKU]int{}
	for _, p := range svc.st.allPlans(orderNo) {
		for _, a := range p.Allocations {
			picked[a.SKU] += a.PickedQuantity
		}
	}
	for _, sh := range svc.st.shipments[orderNo] {
		for _, it := range sh.Items {
			shipped[it.SKU] += it.Quantity
		}
	}
	for _, l := range o.Lines {
		detail.Lines = append(detail.Lines, LineProgress{
			SKU:      l.SKU,
			Required: l.Quantity,
			Picked:   picked[l.SKU],
			Shipped:  shipped[l.SKU],
		})
	}
	return detail, nil
}

// ---------- 辅助 ----------

// normalizeLines 校验订单行：数量必须为正，合并重复 SKU，按 SKU 升序归一化。
func normalizeLines(lines []OrderLine) ([]OrderLine, error) {
	if len(lines) == 0 {
		return nil, ErrInvalidInput
	}
	merged := map[SKU]int{}
	for _, l := range lines {
		if l.SKU == "" || l.Quantity <= 0 {
			return nil, ErrInvalidInput
		}
		merged[l.SKU] += l.Quantity
	}
	out := make([]OrderLine, 0, len(merged))
	for sku, qty := range merged {
		out = append(out, OrderLine{SKU: sku, Quantity: qty})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].SKU < out[j].SKU })
	return out, nil
}

// contentFingerprint 计算订单内容指纹（商品集合 + 数量），用于幂等/冲突判定。
func contentFingerprint(lines []OrderLine) string {
	h := sha256.New()
	for _, l := range lines { // lines 已归一化排序
		h.Write([]byte(l.SKU))
		h.Write([]byte{0})
		var buf [16]byte
		hex.Encode(buf[:], []byte{byte(l.Quantity >> 24), byte(l.Quantity >> 16), byte(l.Quantity >> 8), byte(l.Quantity)})
		h.Write(buf[:])
		h.Write([]byte{0})
	}
	return hex.EncodeToString(h.Sum(nil))
}

func sortWarehouses(ws []WarehouseID) {
	sort.Slice(ws, func(i, j int) bool { return ws[i] < ws[j] })
}

func sortAllocations(a []*Allocation) {
	sort.Slice(a, func(i, j int) bool {
		if a[i].WarehouseID != a[j].WarehouseID {
			return a[i].WarehouseID < a[j].WarehouseID
		}
		return a[i].SKU < a[j].SKU
	})
}

func findAllocation(p *AllocationPlan, w WarehouseID, sku SKU) *Allocation {
	for _, a := range p.Allocations {
		if a.WarehouseID == w && a.SKU == sku {
			return a
		}
	}
	return nil
}

func clonePlan(p *AllocationPlan) *AllocationPlan {
	cp := *p
	cp.Allocations = make([]*Allocation, len(p.Allocations))
	for i, a := range p.Allocations {
		ac := *a
		cp.Allocations[i] = &ac
	}
	return &cp
}

func cloneShipment(s *Shipment) *Shipment {
	cp := *s
	cp.Items = append([]ShipmentItem(nil), s.Items...)
	return &cp
}

// saved 必须在持锁状态调用：落盘快照；未启用持久化时为空操作。
func (svc *Service) saved() error {
	if svc.persist == nil {
		return nil
	}
	return svc.persist(svc.st)
}
