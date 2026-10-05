// Package multimon — F6 (кілька моніторів одночасно): спільна для агента й
// хаба угода про імена потоків.
//
// Монітор 0 їде як і раніше під node_id ноди — для ПК з одним монітором і для
// вимкненої фічі не міняється НІЧОГО. Кожен додатковий монітор i (1..MaxIndex)
// публікується агентом як окрема agent-нога під node_id "<node>#m<i>", і хаб
// роздає його тим самим fanout-ом (GOP-кеш, NACK, бітрейт, гейт) — одна
// nodeSession на трек. Так «fanout per track» не вимагає переписувати машинерію
// одного треку: вона просто працює N разів, незалежно для кожного монітора.
package multimon

import (
	"strconv"
	"strings"
)

// MaxIndex — найбільший індекс додаткового монітора. Стеля тримає перебір у
// хабі (revoke, /control) обмеженим.
const MaxIndex = 15

const sep = "#m"

// NodeID — node_id потоку монітора idx ноди base. idx <= 0 = сама нода.
func NodeID(base string, idx int) string {
	if idx <= 0 {
		return base
	}
	return base + sep + strconv.Itoa(idx)
}

// Parse розбирає "<base>#m<idx>". ok=false для звичайного node_id (у т.ч.
// "#m0", "#m01", індекс поза 1..MaxIndex, порожній base, вкладене
// "a#m1#m2") — такий id лишається звичайною нодою й без власного токена не
// пройде.
func Parse(node string) (base string, idx int, ok bool) {
	i := strings.LastIndex(node, sep)
	if i <= 0 {
		return "", 0, false
	}
	s := node[i+len(sep):]
	if s == "" || s[0] == '0' || len(s) > 2 {
		return "", 0, false
	}
	n, err := strconv.Atoi(s)
	if err != nil || n < 1 || n > MaxIndex {
		return "", 0, false
	}
	base = node[:i]
	if strings.Contains(base, sep) {
		return "", 0, false
	}
	return base, n, true
}
