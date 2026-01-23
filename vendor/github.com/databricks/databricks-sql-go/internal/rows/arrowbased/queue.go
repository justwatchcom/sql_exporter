package arrowbased

import (
	"container/list"
)

type Queue[ItemType any] interface {
	Enqueue(item *ItemType)
	Dequeue() *ItemType
	Clear()
	Len() int
}

func NewQueue[ItemType any]() Queue[ItemType] {
	return &queue[ItemType]{
		items: list.New(),
	}
}

type queue[ItemType any] struct {
	items *list.List
}

var _ Queue[any] = (*queue[any])(nil)

func (q *queue[ItemType]) Enqueue(item *ItemType) {
	q.items.PushBack(item)
}

func (q *queue[ItemType]) Dequeue() *ItemType {
	el := q.items.Front()
	if el == nil {
		return nil
	}
	q.items.Remove(el)

	value, ok := el.Value.(*ItemType)
	if !ok {
		return nil
	}

	return value
}

func (q *queue[ItemType]) Clear() {
	q.items.Init()
}

func (q *queue[ItemType]) Len() int {
	return q.items.Len()
}
