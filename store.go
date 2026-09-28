package goorderfulfillment

// store 是服务使用的内存数据仓储。Service 以单把互斥锁串行化所有写操作，
// 因此 store 本身不再加锁；持久化（JSON 快照）也在持锁期间完成。
type store struct {
	// orders 按外部订单号保存订单聚合。
	orders map[OrderNumber]*Order
	// plans 按订单号保存该订单的全部历史方案版本（版本号从 1 开始）。
	plans map[OrderNumber][]*AllocationPlan
	// stock 仓库 -> SKU -> 库存账面。
	stock map[WarehouseID]map[SKU]*Stock
	// shipments 按订单号保存全部发货记录（追加写）。
	shipments map[OrderNumber][]*Shipment
	// seenReceipts / seenShipments 是回执与发货的幂等去重集合，
	// 键为“业务标识 + 调用方提供的外部 ID”。
	seenReceipts  map[receiptKey]struct{}
	seenShipments map[shipmentKey]struct{}
}

type receiptKey struct {
	order OrderNumber
	id    string
}

type shipmentKey struct {
	order OrderNumber
	id    string
}

func newStore() *store {
	return &store{
		orders:        make(map[OrderNumber]*Order),
		plans:         make(map[OrderNumber][]*AllocationPlan),
		stock:         make(map[WarehouseID]map[SKU]*Stock),
		shipments:     make(map[OrderNumber][]*Shipment),
		seenReceipts:  make(map[receiptKey]struct{}),
		seenShipments: make(map[shipmentKey]struct{}),
	}
}

// getStock 返回指定仓/SKU 的库存账面；create 为 false 时不存在返回 nil。
func (s *store) getStock(w WarehouseID, sku SKU, create bool) *Stock {
	m := s.stock[w]
	if m == nil {
		if !create {
			return nil
		}
		m = make(map[SKU]*Stock)
		s.stock[w] = m
	}
	st := m[sku]
	if st == nil && create {
		st = &Stock{WarehouseID: w, SKU: sku}
		m[sku] = st
	}
	return st
}

// knownWarehouses 按 ID 升序返回所有有库存记录的仓库，保证拆分方案稳定可复现。
func (s *store) knownWarehouses() []WarehouseID {
	ws := make([]WarehouseID, 0, len(s.stock))
	for w := range s.stock {
		ws = append(ws, w)
	}
	sortWarehouses(ws)
	return ws
}

func (s *store) getOrder(n OrderNumber) (*Order, error) {
	o := s.orders[n]
	if o == nil {
		return nil, ErrOrderNotFound
	}
	return o, nil
}

// getPlan 返回指定版本方案；version <= 0 表示取最新版本。
func (s *store) getPlan(n OrderNumber, version int) (*AllocationPlan, error) {
	plans := s.plans[n]
	if len(plans) == 0 {
		return nil, ErrPlanNotFound
	}
	if version <= 0 || version > len(plans) {
		if version > len(plans) {
			return nil, ErrPlanNotFound
		}
		return plans[len(plans)-1], nil
	}
	return plans[version-1], nil
}

// appendPlan 追加一个新版本，并维护订单上的当前版本号。
func (s *store) appendPlan(plan *AllocationPlan) {
	s.plans[plan.OrderNumber] = append(s.plans[plan.OrderNumber], plan)
	if o := s.orders[plan.OrderNumber]; o != nil {
		o.CurrentVersion = len(s.plans[plan.OrderNumber])
	}
}

// allPlans 返回某订单的全部方案版本（按版本号升序）。
func (s *store) allPlans(n OrderNumber) []*AllocationPlan {
	return s.plans[n]
}
