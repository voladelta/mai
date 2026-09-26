package todo

type Task struct {
	ID    int
	Title string
	Done  bool
}

type List struct {
	Tasks []Task
	next  int
}

func (list *List) Add(title string) int {
	list.next++
	list.Tasks = append(list.Tasks, Task{ID: list.next, Title: title})
	return list.next
}
