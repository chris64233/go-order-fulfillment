package goorderfulfillment

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
)

// snapshot 是内存仓储的全量 JSON 持久化格式。
//
// 每次成功写操作（确认、拣货回执、重配、发货、取消）后，在服务锁内把全量状态
// 原子写入文件（先写临时文件再 rename）。这样：
//   - 每一版拆分方案都会随快照完整保留；
//   - “订单状态 + 库存占用 + 方案版本 + 发货记录”要么是改动前、要么是改动后，
//     崩溃恢复不会读到交错的中间态；
//   - 进程重启后 LoadServiceFromFile 可以无损恢复继续履约。
type snapshot struct {
	Orders    []*Order          `json:"orders"`
	Plans     []*AllocationPlan `json:"plans"`
	Stocks    []*Stock          `json:"stocks"`
	Shipments []*Shipment       `json:"shipments"`
	// 幂等去重集合也随快照保存，恢复后重复回执/发货仍会被拒绝。
	SeenReceipts  []string `json:"seen_receipts,omitempty"`
	SeenShipments []string `json:"seen_shipments,omitempty"`
}

// EnableFileSnapshot 启用 JSON 文件快照：立即落一版当前状态，
// 之后每次成功写操作都会原子覆盖该文件。
func (svc *Service) EnableFileSnapshot(path string) error {
	svc.mu.Lock()
	defer svc.mu.Unlock()
	svc.persist = func(st *store) error {
		return writeSnapshot(path, st)
	}
	return svc.saved()
}

// NewServiceFromFile 从快照文件恢复一个服务；文件不存在时返回一个全新的空服务
// （不报错），方便调用方用固定路径“首次创建 / 后续恢复”。
func NewServiceFromFile(path string) (*Service, bool, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			svc := NewService()
			if err := svc.EnableFileSnapshot(path); err != nil {
				return nil, false, err
			}
			return svc, false, nil
		}
		return nil, false, err
	}
	var snap snapshot
	if err := json.Unmarshal(data, &snap); err != nil {
		return nil, false, fmt.Errorf("parse snapshot %s: %w", path, err)
	}
	st := newStore()
	for _, o := range snap.Orders {
		st.orders[o.OrderNumber] = o
	}
	for _, p := range snap.Plans {
		st.plans[p.OrderNumber] = append(st.plans[p.OrderNumber], p)
	}
	for _, s := range snap.Stocks {
		if st.stock[s.WarehouseID] == nil {
			st.stock[s.WarehouseID] = map[SKU]*Stock{}
		}
		st.stock[s.WarehouseID][s.SKU] = s
	}
	for _, sh := range snap.Shipments {
		st.shipments[sh.OrderNumber] = append(st.shipments[sh.OrderNumber], sh)
	}
	for _, k := range snap.SeenReceipts {
		var order, id string
		if _, err := fmt.Sscanf(k, "%q %q", &order, &id); err == nil {
			st.seenReceipts[receiptKey{OrderNumber(order), id}] = struct{}{}
		}
	}
	for _, k := range snap.SeenShipments {
		var order, id string
		if _, err := fmt.Sscanf(k, "%q %q", &order, &id); err == nil {
			st.seenShipments[shipmentKey{OrderNumber(order), id}] = struct{}{}
		}
	}
	svc := &Service{st: st}
	if err := svc.EnableFileSnapshot(path); err != nil {
		return nil, true, err
	}
	return svc, true, nil
}

// writeSnapshot 在持锁状态下把仓储序列化为确定性 JSON 并原子替换目标文件。
func writeSnapshot(path string, st *store) error {
	snap := snapshot{}

	orderNos := make([]OrderNumber, 0, len(st.orders))
	for n := range st.orders {
		orderNos = append(orderNos, n)
	}
	sort.Slice(orderNos, func(i, j int) bool { return orderNos[i] < orderNos[j] })
	for _, n := range orderNos {
		snap.Orders = append(snap.Orders, st.orders[n])
		snap.Plans = append(snap.Plans, st.plans[n]...)
		snap.Shipments = append(snap.Shipments, st.shipments[n]...)
	}
	for _, w := range st.knownWarehouses() {
		skus := make([]SKU, 0, len(st.stock[w]))
		for sku := range st.stock[w] {
			skus = append(skus, sku)
		}
		sort.Slice(skus, func(i, j int) bool { return skus[i] < skus[j] })
		for _, sku := range skus {
			snap.Stocks = append(snap.Stocks, st.stock[w][sku])
		}
	}
	for k := range st.seenReceipts {
		snap.SeenReceipts = append(snap.SeenReceipts,
			fmt.Sprintf("%q %q", string(k.order), k.id))
	}
	sort.Strings(snap.SeenReceipts)
	for k := range st.seenShipments {
		snap.SeenShipments = append(snap.SeenShipments,
			fmt.Sprintf("%q %q", string(k.order), k.id))
	}
	sort.Strings(snap.SeenShipments)

	data, err := json.MarshalIndent(&snap, "", "  ")
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, data, 0o644); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}
